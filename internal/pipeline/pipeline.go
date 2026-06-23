package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/devices"
	"racecast-emitter/internal/logger"
)

const recordsDir = "records"

func envInt(key string, defaultVal int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			return v
		}
		logger.Warn("Invalid environment variable %s, using default: %d", key, defaultVal)
	}
	return defaultVal
}

func videoBitrate() int { return envInt("RC_VIDEO_BITRATE", 12_000_000) }
func audioBitrate() int { return envInt("RC_AUDIO_BITRATE", 192_000) }

// intraRefreshPeriod returns the intra-refresh period in frames (0 = disabled).
func intraRefreshPeriod() int { return envInt("RC_VIDEO_INTRA_REFRESH", 0) }

// srtPortStart returns RC_SRT_PORT; all streams share the same port (differentiated by SRT streamid).
func srtPortStart() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RC_SRT_PORT"))); err == nil && n > 0 {
		return n
	}
	logger.Warn("RC_SRT_PORT not set or invalid — defaulting to 9000")
	return 9000
}

func sanitize(name string) string {
	return strings.NewReplacer(" ", "_", "/", "-").Replace(name)
}

type Slot struct {
	mu       sync.Mutex
	gst      *GstPipeline
	notFound bool
}

func (s *Slot) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gst != nil
}

// activate registers gp in the slot, wires wg.Done() and background cleanup.
// If ctx is already cancelled, frees the pipeline without adding to wg.
func (s *Slot) activate(label string, gp *GstPipeline, wg *sync.WaitGroup) {
	// Context already cancelled: skip wg registration.
	select {
	case <-gp.ctx.Done():
		logger.Info("[%s] Context cancelled during startup -- pipeline discarded", label)
		go func() { gp.SetNull(); gp.Free() }()
		return
	default:
	}
	s.mu.Lock()
	s.gst = gp
	s.mu.Unlock()
	logger.Info("[%s] GStreamer pipeline started", label)
	wg.Add(1)
	go func() {
		defer wg.Done()
		gp.wg.Wait()
		logger.Info("[%s] GStreamer pipeline stopped", label)
		s.mu.Lock()
		if s.gst == gp {
			s.gst = nil
		}
		s.mu.Unlock()
		// SetNull/Free in background: srtsink may block on reconnect.
		go func() {
			gp.SetNull()
			gp.Free()
		}()
	}()
}

func (s *Slot) SendEOS() {
	s.mu.Lock()
	gp := s.gst
	s.mu.Unlock()
	if gp != nil {
		gp.SendEOS()
		// Cancel unblocks watchBus(): srtsink in reconnect mode never produces EOS.
		gp.Cancel()
	}
}

// ForceIDR forces an IDR frame on the AV1 encoder. No-op if not running.
func (s *Slot) ForceIDR(encoderName string) {
	s.mu.Lock()
	gp := s.gst
	s.mu.Unlock()
	if gp != nil {
		gp.ForceIDR(encoderName)
	}
}

// SetStreamValve opens (drop=false) or closes (drop=true) the streaming valve.
// When opening, forces an IDR frame so the receiver can decode immediately.
// No-op if the pipeline is not running or has no streaming branch.
func (s *Slot) SetStreamValve(drop bool) {
	s.mu.Lock()
	gp := s.gst
	s.mu.Unlock()
	if gp == nil {
		return
	}
	gp.SetValve("streamvalve", drop)
	if !drop {
		gp.ForceIDR("avenc") // no-op on audio pipelines (element not found)
	}
}

// SetRecordValve opens (drop=false) or closes (drop=true) the recording valve.
// No-op if the pipeline is not running or has no record valve (e.g. record-only mode).
func (s *Slot) SetRecordValve(drop bool) {
	s.mu.Lock()
	gp := s.gst
	s.mu.Unlock()
	if gp == nil {
		return
	}
	gp.SetValve("recordvalve", drop)
}

type PollOptions struct {
	Record bool
	Stream bool
}

func Poll(ctx context.Context, opts PollOptions, cfg *config.Config, cameraSlots, micSlots map[string]*Slot, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	// All streams share one SRT port; receiver distinguishes by streamid.
	srtPort := srtPortStart()

	// startEntry holds the data needed to start a pipeline.
	type startEntry struct {
		label       string
		pipelineStr string
		slot        *Slot
		maxBitrate  int  // 0 for record-only or audio entries
		isStreaming bool // true when the entry has an active SRT sink
		doRecord    bool // true when the record branch should be active
	}

	var entries []startEntry

	for i := range cfg.Cameras {
		cam := cfg.Cameras[i]
		if cam.Disabled {
			continue
		}
		s := cameraSlots[cam.UID]
		if s == nil || s.IsRunning() {
			continue
		}
		doStream := opts.Stream && cam.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		dev, err := devices.FindVideo(cam.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[camera:%s] Not found (UID: %s), waiting...", cam.Name, cam.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false
		// Always provide a valid outputPath: "/dev/null" when recording is disabled
		// so the record branch (valve closed) has a valid filesink location.
		outputPath := "/dev/null"
		if opts.Record {
			now := time.Now()
			dir := filepath.Join(recordsDir, now.Format("2006-01-02"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("[camera:%s] Failed to create directory: %v", cam.Name, err)
				continue
			}
			outputPath = filepath.Join(dir, fmt.Sprintf("%s_%s_video.mp4", now.Format("15-04-05"), sanitize(cam.Name)))
		}
		port := 0
		if doStream {
			port = srtPort
		}
		label := "camera:" + sanitize(cam.Name)
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s -> record + AV1/SRT stream (port %d)", label, dev, port)
		case opts.Record:
			logger.Info("[%s] %s -> H.264 record", label, dev)
		default:
			logger.Info("[%s] %s -> AV1/SRT stream (port %d)", label, dev, port)
		}
		entries = append(entries, startEntry{
			label:       label,
			pipelineStr: BuildVideoStr(cam, dev, outputPath, port),
			slot:        s,
			maxBitrate:  cam.StreamBitrate(),
			isStreaming: doStream,
			doRecord:    opts.Record,
		})
	}

	for i := range cfg.Microphones {
		mic := cfg.Microphones[i]
		if mic.Disabled {
			continue
		}
		s := micSlots[mic.UID]
		if s == nil || s.IsRunning() {
			continue
		}
		doStream := opts.Stream && mic.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		alsaDev, err := devices.FindALSA(mic.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[mic:%s] Not found (UID: %s), waiting...", mic.Name, mic.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false
		// For audio, outputPath="" means stream-only (no mp4mux/blacktrack).
		outputPath := ""
		if opts.Record {
			now := time.Now()
			dir := filepath.Join(recordsDir, now.Format("2006-01-02"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("[mic:%s] Failed to create directory: %v", mic.Name, err)
				continue
			}
			outputPath = filepath.Join(dir, fmt.Sprintf("%s_%s_audio.mp4", now.Format("15-04-05"), sanitize(mic.Name)))
		}
		port := 0
		if doStream {
			port = srtPort
		}
		label := "mic:" + sanitize(mic.Name)
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s -> record + Opus/SRT stream (port %d)", label, alsaDev, port)
		case opts.Record:
			logger.Info("[%s] %s -> AAC record", label, alsaDev)
		default:
			logger.Info("[%s] %s -> Opus/SRT stream (port %d)", label, alsaDev, port)
		}
		entries = append(entries, startEntry{
			label:       label,
			pipelineStr: BuildAudioStr(mic, alsaDev, outputPath, port),
			slot:        s,
			doRecord:    opts.Record,
			isStreaming: doStream,
		})
	}

	if len(entries) == 0 {
		return
	}

	// Phase 1: create all GStreamer pipelines (gst_parse_launch).
	type prepEntry struct {
		startEntry
		gp *GstPipeline
	}
	var preps []prepEntry
	for _, e := range entries {
		e := e
		s := e.slot
		s.mu.Lock()
		ok := ctx.Err() == nil && s.gst == nil
		s.mu.Unlock()
		if !ok {
			continue
		}
		gp, err := newGstPipeline(ctx, e.pipelineStr)
		if err != nil {
			logger.Error("[%s] Failed to create pipeline: %v", e.label, err)
			continue
		}
		gp.SetOnError(func() {
			s.mu.Lock()
			if s.gst == gp {
				gp.SetNull()
				gp.Free()
				s.gst = nil
			}
			s.mu.Unlock()
			logger.Warn("[%s] Pipeline error -- device disconnected?", e.label)
		})
		preps = append(preps, prepEntry{e, gp})
	}

	if len(preps) == 0 {
		return
	}

	// Start all pipelines in parallel; activate each on GST_STATE_PLAYING.
	// Audio (~100 ms) starts while camera encoders (3–5 s) are still warming up.
	gpList := make([]*GstPipeline, len(preps))
	for i, pe := range preps {
		gpList[i] = pe.gp
	}
	StartEach(gpList, func(i int, err error) {
		pe := preps[i]
		if err != nil {
			logger.Error("[%s] Failed to start pipeline: %v", pe.label, err)
			go pe.gp.Free() // run in background: Free acquires silenceMu
			return
		}
		pe.slot.activate(pe.label, pe.gp, wg)
		// Apply initial valve states based on run mode.
		if !pe.doRecord {
			pe.slot.SetRecordValve(true) // close record branch (stream-only mode)
		}
		if !pe.isStreaming {
			pe.slot.SetStreamValve(true) // close stream branch (no SRT configured)
			return
		}
		// Intra-refresh: set on AV1 encoder after startup.
		if period := intraRefreshPeriod(); period > 0 {
			pe.gp.TrySetIntraRefresh("avenc", period)
			logger.Info("[%s] Intra-refresh enabled (period=%d frames)", pe.label, period)
		}
		// Local ABR: reads srtsink statistics directly — no network round-trip.
		minBR := pe.maxBitrate / 5
		pe.gp.WatchLocalStats("avenc", "srtsink", minBR, pe.maxBitrate)
	})
}
