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
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
		logger.Warn("Invalid environment variable %s, using default: %d", key, defaultVal)
	}
	return defaultVal
}

func videoBitrate() int { return envInt("RC_VIDEO_BITRATE", 12_000_000) }
func audioBitrate() int { return envInt("RC_AUDIO_BITRATE", 192_000) }

// srtPortStart returns the SRT port from RC_SRT_PORT.
// All streams (cameras and microphones) connect on the same port;
// the receiver differentiates connections by SRT streamid ("name:source").
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

// activate registers an already-started GstPipeline in the slot and launches
// a goroutine that signals pipeline stop via wg.Done(), then releases GStreamer
// resources in the background without blocking the shutdown path.
// If ctx is already cancelled (e.g. Ctrl+C during StartAll), the pipeline is
// freed in the background without adding to wg.
func (s *Slot) activate(label string, gp *GstPipeline, wg *sync.WaitGroup) {
	// Ctx cancelled during StartAll: don't register in wg.
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
		// SetNull and Free in background: don't block wg.Done().
		// gst_element_set_state(NULL) may block while srtsink finishes
		// an in-progress reconnect attempt. The slot is already nil so
		// a new pipeline can be created immediately.
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
		// Cancel the internal context to unblock watchBus() immediately.
		// Without this, watchBus() waits for an EOS bus message that srtsink
		// in reconnect mode never produces, stalling wg.Wait() indefinitely.
		gp.Cancel()
	}
}

type PollOptions struct {
	Record bool
	Stream bool
}

func Poll(ctx context.Context, opts PollOptions, cfg *config.Config, cameraSlots, micSlots map[string]*Slot, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	// SRT port: all streams connect on the same port.
	// The receiver differentiates connections by SRT streamid.
	srtPort := srtPortStart()
	cameraPort := map[string]int{} // uid → SRT port
	if opts.Stream {
		for _, cam := range cfg.Cameras {
			if cam.Disabled || !cam.HasStream() {
				continue
			}
			cameraPort[cam.UID] = srtPort
		}
	}
	micPort := map[string]int{} // uid → SRT port
	if opts.Stream {
		for _, mic := range cfg.Microphones {
			if !mic.HasStream() {
				continue
			}
			micPort[mic.UID] = srtPort
		}
	}

	// startEntry regroupe tout ce qui est nécessaire pour démarrer un pipeline.
	type startEntry struct {
		label       string
		pipelineStr string
		slot        *Slot
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
		outputPath := ""
		if opts.Record {
			now := time.Now()
			dir := filepath.Join(recordsDir, now.Format("2006-01-02"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("[camera:%s] Failed to create directory: %v", cam.Name, err)
				continue
			}
			outputPath = filepath.Join(dir, fmt.Sprintf("%s_%s_video.mp4", now.Format("15-04-05"), sanitize(cam.Name)))
		}
		label := "camera:" + sanitize(cam.Name)
		port := cameraPort[cam.UID]
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
			pipelineStr: BuildVideoStr(cam, dev, outputPath, doStream, port),
			slot:        s,
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
		label := "mic:" + sanitize(mic.Name)
		port := micPort[mic.UID]
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
			pipelineStr: BuildAudioStr(mic, alsaDev, outputPath, doStream, port),
			slot:        s,
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

	// Phase 2: start all pipelines in parallel under a single global silencing window.
	gpList := make([]*GstPipeline, len(preps))
	for i, pe := range preps {
		gpList[i] = pe.gp
	}
	errs := StartAll(gpList)

	// Phase 3: activate slots.
	for i, pe := range preps {
		if errs[i] != nil {
			logger.Error("[%s] Failed to start pipeline: %v", pe.label, errs[i])
			pe.gp.Free()
			continue
		}
		pe.slot.activate(pe.label, pe.gp, wg)
	}
}
