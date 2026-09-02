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
		logger.Warn("[config] Invalid environment variable %s, using default: %d", key, defaultVal)
	}
	return defaultVal
}

func videoBitrate() int { return envInt("RC_VIDEO_BITRATE", 12_000_000) }

// intraRefreshPeriod returns the intra-refresh period in frames (0 = disabled).
func intraRefreshPeriod() int { return envInt("RC_VIDEO_INTRA_REFRESH", 0) }

// srtPortStart returns RC_SRT_PORT; all streams share the same port (differentiated by SRT streamid).
func srtPortStart() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RC_SRT_PORT"))); err == nil && n > 0 {
		return n
	}
	logger.Warn("[srt] RC_SRT_PORT not set or invalid — defaulting to 9000")
	return 9000
}

func sanitize(name string) string {
	return strings.NewReplacer(" ", "_", "/", "-").Replace(name)
}

// Slot manages the three independent GStreamer pipelines for a single source
// (camera or microphone):
//
//   - source: always running while the device is connected; captures and
//     distributes raw frames to both consumer channels via inter elements.
//   - record: created per recording session (started at launch or on button
//     press); reads from the record inter channel and writes a fragmented MP4.
//   - stream: persistent while streaming is configured; reads from the stream
//     inter channel and encodes to AV1/Opus → SRT. Paused during modem
//     outages and resumed instantly when connectivity returns.
type Slot struct {
	mu       sync.Mutex
	source   *GstPipeline
	record   *GstPipeline
	stream   *GstPipeline
	notFound bool
	// starting marks a field (keyed by its own address — &s.source etc.,
	// stable for the Slot's lifetime) whose pipeline is currently being
	// constructed/launched: gst_element_set_state (in startInner) can block
	// for a while, and *field itself isn't set until activatePipeline runs
	// afterward — without this, a slow start left *field == nil looks
	// identical to "nothing launching yet" to startEntries's gating check,
	// letting a second Poll() cycle launch a duplicate pipeline against the
	// same physical device before the first one finishes.
	starting map[**GstPipeline]bool
}

func (s *Slot) IsSourceRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source != nil
}

func (s *Slot) IsRecordRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record != nil
}

func (s *Slot) IsStreamRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream != nil
}

// clearStarting marks field's launch attempt as finished (success or
// failure) — see the starting field's doc comment.
func (s *Slot) clearStarting(field **GstPipeline) {
	s.mu.Lock()
	delete(s.starting, field)
	s.mu.Unlock()
}

// activatePipeline registers gp in the slot field pointed to by field, adds a
// wg entry, and spawns a goroutine that calls wg.Done() once the pipeline stops.
// If the control context was cancelled during startup, the pipeline is discarded.
// A second call with the same field while one is already active is a no-op.
// onStopped, if non-nil, runs after SetNull() completes — i.e. once the
// pipeline's output file is guaranteed closed (filesink closes its fd during
// the PAUSED→READY transition SetNull triggers) — for post-processing that
// needs the finished file, such as bwf.go's bext injection on a completed
// audio recording. Most callers pass nil.
func (s *Slot) activatePipeline(label string, gp *GstPipeline, field **GstPipeline, wg *sync.WaitGroup, onStopped func()) {
	select {
	case <-gp.controlCtx.Done():
		logger.Info("[%s] Context cancelled during startup — pipeline discarded", label)
		go func() { gp.SetNull(); gp.Free() }()
		return
	default:
	}
	s.mu.Lock()
	if *field != nil {
		// Another goroutine already activated this pipeline type.
		s.mu.Unlock()
		go func() { gp.SetNull(); gp.Free() }()
		return
	}
	*field = gp
	s.mu.Unlock()
	logger.Info("[%s] GStreamer pipeline started", label)
	trackPipelineLifecycle(label, gp, field, s, wg, onStopped)
}

// trackPipelineLifecycle spawns a goroutine that waits for gp to stop, then
// clears *field and cleans up. onStopped, if non-nil, runs after SetNull()
// completes — see activatePipeline's doc comment for why.
func trackPipelineLifecycle(label string, gp *GstPipeline, field **GstPipeline, s *Slot, wg *sync.WaitGroup, onStopped func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		gp.wg.Wait()
		logger.Info("[%s] GStreamer pipeline stopped", label)
		s.mu.Lock()
		*field = nil
		s.mu.Unlock()
		go func() {
			gp.SetNull()
			gp.Free()
			if onStopped != nil {
				onStopped()
			}
		}()
	}()
}

// Stop gracefully shuts down all pipelines for this slot:
//   - record pipeline receives EOS so that the MP4 container is finalised before exit.
//   - source and stream pipelines are cancelled immediately (no persistent state).
func (s *Slot) Stop() {
	s.mu.Lock()
	src := s.source
	rec := s.record
	str := s.stream
	s.mu.Unlock()

	// Cancel source and stream immediately — nothing to finalise.
	if src != nil {
		src.Cancel()
	}
	if str != nil {
		str.Cancel()
	}
	// Send EOS to record so that mp4mux flushes and filesink closes cleanly.
	// EOS is injected directly at intervideosrc/interaudiosrc (the pipeline's
	// source element), so it propagates without needing the source pipeline.
	if rec != nil {
		rec.SendEOS()
	}
}

// PauseStream sets the streaming pipeline to GST_STATE_PAUSED.
// The AV1/Opus encoder stops consuming GPU resources; srtsink drops the
// connection. The pipeline can be resumed instantly via ResumeStream.
func (s *Slot) PauseStream() {
	s.mu.Lock()
	gp := s.stream
	s.mu.Unlock()
	if gp != nil {
		gp.Pause()
	}
}

// ResumeStream transitions the streaming pipeline back to GST_STATE_PLAYING.
// An IDR is always forced on link recovery: the receiver recreates its
// GStreamer pipeline on reconnect and needs a clean keyframe to start decoding,
// regardless of whether intra-refresh is active. The one-time bitrate spike is
// acceptable since the link has just recovered.
func (s *Slot) ResumeStream() {
	s.mu.Lock()
	gp := s.stream
	s.mu.Unlock()
	if gp != nil {
		gp.Play()
		gp.ForceIDR("avenc") // no-op on audio pipelines (element not found)
	}
}

// ForceIDR requests an immediate IDR frame on the stream pipeline's encoder.
func (s *Slot) ForceIDR(encoderName string) {
	s.mu.Lock()
	gp := s.stream
	s.mu.Unlock()
	if gp != nil {
		gp.ForceIDR(encoderName)
	}
}

// ── Poll ─────────────────────────────────────────────────────────────────────

type PollOptions struct {
	Record bool
	Stream bool
	// NotifyClose is called when a source pipeline errors (device disconnect).
	// The argument is the camera or microphone name. Optional.
	NotifyClose func(name string)
	// Bandwidth is where camera stream entries register for shared video
	// bitrate allocation instead of running feedback.go's old independent
	// per-stream ABR ceiling — see bandwidth.go. Required whenever Stream is
	// true and any camera has streaming enabled; audio is unaffected.
	Bandwidth *BandwidthCoordinator
}

// Poll scans all configured sources and starts any pipeline that is not yet
// running. It proceeds in two sequential phases so that record and stream
// pipelines are only launched once the capture pipeline for their device is
// confirmed running:
//
//  1. Start capture (source) pipelines for any device not yet capturing.
//  2. Start record/stream (consumer) pipelines only for slots whose source is
//     now running — either because it was already active or because phase 1
//     just started it successfully.
func Poll(ctx context.Context, opts PollOptions, cfg *config.Config, cameraSlots, micSlots map[string]*Slot, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	srtPort := srtPortStart()

	type entry struct {
		label       string
		str         string
		slot        *Slot
		field       **GstPipeline
		isStream    bool // run the ABR post-start block (WatchLocalStats), video or audio
		videoOnly   bool // also try intra-refresh, meaningless for the Opus audio encoder
		encoderName string
		maxBitrate  int
		// streamName is set on source entries so the onError callback can notify
		// the receiver that the stream was closed intentionally.
		streamName string
		// onStopped, if set, runs once this pipeline has fully stopped and its
		// output file is closed — see activatePipeline.
		onStopped func()
		// onStarted, if set, runs once this pipeline's state change to
		// PLAYING has been successfully issued (see StartEach) — i.e. as
		// close to "recording actually began" as this layer can observe
		// without a first-buffer pad probe. Used by mic record entries to
		// stamp BWF TimeReference at start time rather than at
		// entry-construction time, which can run well before this
		// particular pipeline's turn in the batch (see injectBWFTimeReference).
		onStarted func()
		// cameraName, isMain and streamWidth/Height/Framerate are set on
		// camera stream entries (videoOnly) only, for
		// BandwidthCoordinator.Register — streamWidth/Height/Framerate let
		// changeTier compute a reduced resolution to switch to live.
		cameraName                                 string
		isMain                                     bool
		streamWidth, streamHeight, streamFramerate int
	}

	// startEntries creates GStreamer pipelines for all entries in the slice and
	// starts them in parallel under a single silence window.
	startEntries := func(entries []entry) {
		if len(entries) == 0 {
			return
		}
		type prepEntry struct {
			entry
			gp *GstPipeline
		}
		var preps []prepEntry
		for _, e := range entries {
			e := e
			s := e.slot
			s.mu.Lock()
			ok := ctx.Err() == nil && *e.field == nil && !s.starting[e.field]
			if ok {
				if s.starting == nil {
					s.starting = make(map[**GstPipeline]bool)
				}
				s.starting[e.field] = true
			}
			s.mu.Unlock()
			if !ok {
				continue
			}
			gp, err := newGstPipeline(ctx, e.str)
			if err != nil {
				logger.Error("[%s] Failed to create pipeline: %v", e.label, err)
				s.clearStarting(e.field)
				continue
			}
			if e.isStream {
				// Must happen before StartEach below transitions this pipeline
				// to PLAYING, so every buffer — not just ones lucky enough to
				// arrive after the probe is attached — carries the capture
				// timestamp RaceCast-Receiver now expects on every SRT media
				// message. Record pipelines have no srtsink, so this is a
				// harmless no-op for them (isStream is false there).
				gp.AttachTimestampPrefix("srtsink")
			}
			field := e.field
			streamName := e.streamName
			gp.SetOnError(func() {
				// Notify the receiver immediately so it skips the grace period
				// and unpublishes the LiveKit track without delay.
				if opts.NotifyClose != nil && streamName != "" {
					opts.NotifyClose(streamName)
				}
				s.mu.Lock()
				if *field == gp {
					*field = nil
				}
				s.mu.Unlock()
				go func() { gp.SetNull(); gp.Free() }()
				logger.Warn("[%s] Pipeline error — device disconnected?", e.label)
			})
			preps = append(preps, prepEntry{e, gp})
		}
		if len(preps) == 0 {
			return
		}
		gpList := make([]*GstPipeline, len(preps))
		for i, pe := range preps {
			gpList[i] = pe.gp
		}
		StartEach(gpList, func(i int, err error) {
			pe := preps[i]
			defer pe.slot.clearStarting(pe.field)
			if err != nil {
				logger.Error("[%s] Failed to start pipeline: %v", pe.label, err)
				go pe.gp.Free()
				return
			}
			pe.slot.activatePipeline(pe.label, pe.gp, pe.field, wg, pe.onStopped)
			if pe.onStarted != nil {
				pe.onStarted()
			}
			if !pe.isStream {
				return
			}
			// Stream pipeline post-start: intra-refresh (video only), then ABR.
			minBR := pe.maxBitrate / 5
			if pe.videoOnly {
				if period := intraRefreshPeriod(); period > 0 {
					if pe.gp.TrySetIntraRefresh(pe.encoderName, period) {
						logger.Info("[%s] Intra-refresh enabled (period=%d frames)", pe.label, period)
					} else {
						logger.Warn("[%s] Intra-refresh requested but encoder element not found", pe.label)
					}
				}
				// Video shares one bandwidth budget across all cameras (bandwidth.go)
				// instead of running its own independent ABR ceiling — see its
				// package comment for why. Audio (below) is unaffected.
				if opts.Bandwidth != nil {
					opts.Bandwidth.Register(RegisterOptions{
						Name:        pe.cameraName,
						IsMain:      pe.isMain,
						Slot:        pe.slot,
						Pipeline:    pe.gp,
						EncoderName: pe.encoderName,
						MinBitrate:  minBR,
						MaxBitrate:  pe.maxBitrate,
						Width:       pe.streamWidth,
						Height:      pe.streamHeight,
						Framerate:   pe.streamFramerate,
						OnStopped:   pe.onStopped,
					})
				} else {
					logger.Warn("[%s] No BandwidthCoordinator configured — video stream has no ABR", pe.label)
				}
				return
			}
			pe.gp.WatchLocalStats(pe.encoderName, "srtsink", minBR, pe.maxBitrate)
		})
	}

	// ── Phase 1: start capture pipelines for newly discovered devices ─────────
	var srcEntries []entry

	for i := range cfg.Cameras {
		cam := cfg.Cameras[i]
		if cam.Disabled {
			continue
		}
		s := cameraSlots[cam.UID]
		if s == nil || s.IsSourceRunning() {
			continue
		}
		doStream := opts.Stream && cam.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		label := "camera:" + sanitize(cam.Name)
		dev, err := devices.FindVideo(cam.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[%s] Not found (UID: %s), waiting...", label, cam.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s → H.264 record + AV1/SRT stream (port %d)", label, dev, srtPort)
		case opts.Record:
			logger.Info("[%s] %s → H.264 record", label, dev)
		default:
			logger.Info("[%s] %s → AV1/SRT stream (port %d)", label, dev, srtPort)
		}
		srcEntries = append(srcEntries, entry{
			label:      label + ":source",
			str:        BuildVideoSourceStr(cam, dev),
			slot:       s,
			field:      &s.source,
			streamName: cam.Name,
		})
	}

	for i := range cfg.Microphones {
		mic := cfg.Microphones[i]
		if mic.Disabled {
			continue
		}
		s := micSlots[mic.UID]
		if s == nil || s.IsSourceRunning() {
			continue
		}
		doStream := opts.Stream && mic.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		label := "mic:" + sanitize(mic.Name)
		alsaDev, err := devices.FindALSA(mic.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[%s] Not found (UID: %s), waiting...", label, mic.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s → AAC record + Opus/SRT stream (port %d)", label, alsaDev, srtPort)
		case opts.Record:
			logger.Info("[%s] %s → AAC record", label, alsaDev)
		default:
			logger.Info("[%s] %s → Opus/SRT stream (port %d)", label, alsaDev, srtPort)
		}
		srcEntries = append(srcEntries, entry{
			label:      label + ":source",
			str:        BuildAudioSourceStr(mic, alsaDev),
			slot:       s,
			field:      &s.source,
			streamName: mic.Name,
		})
	}

	startEntries(srcEntries)

	// ── Phase 2: start record/stream pipelines for slots with a running source ─
	// After startEntries returns, activatePipeline has been called for every
	// source that started successfully, so IsSourceRunning() reflects the
	// current state and gates the consumer pipelines correctly.
	var consEntries []entry

	for i := range cfg.Cameras {
		cam := cfg.Cameras[i]
		if cam.Disabled {
			continue
		}
		s := cameraSlots[cam.UID]
		if s == nil || !s.IsSourceRunning() {
			continue
		}
		doStream := opts.Stream && cam.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		label := "camera:" + sanitize(cam.Name)

		if opts.Record && !s.IsRecordRunning() {
			now := time.Now()
			dir := filepath.Join(recordsDir, now.Format("2006-01-02"), "video")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("[%s] Failed to create directory: %v", label, err)
				continue
			}
			// .mov, not .mp4: BuildVideoRecordStr muxes with qtmux (needed for
			// the embedded SMPTE timecode track — see its comment) which
			// produces a QuickTime file, not a strict-profile MP4.
			outputPath := filepath.Join(dir, fmt.Sprintf("%s_%s_video.mov",
				now.Format("15-04-05"), sanitize(cam.Name)))
			consEntries = append(consEntries, entry{
				label: label + ":record",
				str:   BuildVideoRecordStr(cam, outputPath),
				slot:  s,
				field: &s.record,
			})
		}

		if doStream && !s.IsStreamRunning() {
			camName := cam.Name
			streamLabel := label + ":stream"

			consEntries = append(consEntries, entry{
				label:           streamLabel,
				str:             BuildVideoStreamStr(cam, srtPort),
				slot:            s,
				field:           &s.stream,
				isStream:        true,
				videoOnly:       true,
				encoderName:     "avenc",
				maxBitrate:      cam.StreamBitrate(),
				cameraName:      camName,
				isMain:          cam.Main,
				streamWidth:     cam.StreamWidth(),
				streamHeight:    cam.StreamHeight(),
				streamFramerate: cam.StreamFramerate(),
				onStopped: func() {
					if opts.Bandwidth != nil {
						opts.Bandwidth.Unregister(camName)
					}
				},
			})
		}
	}

	for i := range cfg.Microphones {
		mic := cfg.Microphones[i]
		if mic.Disabled {
			continue
		}
		s := micSlots[mic.UID]
		if s == nil || !s.IsSourceRunning() {
			continue
		}
		doStream := opts.Stream && mic.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		label := "mic:" + sanitize(mic.Name)

		if opts.Record && !s.IsRecordRunning() {
			now := time.Now()
			dir := filepath.Join(recordsDir, now.Format("2006-01-02"), "audio")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("[%s] Failed to create directory: %v", label, err)
				continue
			}
			// .wav, not .mp4: BuildAudioRecordStr now writes plain PCM via
			// wavenc — see its comment — with a BWF "bext" timecode chunk
			// patched in by injectBWFTimeReference once recording stops,
			// rather than muxing AAC with a synthetic black video track.
			outputPath := filepath.Join(dir, fmt.Sprintf("%s_%s_audio.wav",
				now.Format("15-04-05"), sanitize(mic.Name)))
			// startedAt is set by onStarted, not captured here: this loop runs
			// over every camera and mic before startEntries actually issues
			// any pipeline's state change, so "now" at entry-construction
			// time can run well ahead of when this particular pipeline
			// starts — see onStarted's doc comment.
			var startedAt time.Time
			consEntries = append(consEntries, entry{
				label: label + ":record",
				str:   BuildAudioRecordStr(mic, outputPath),
				slot:  s,
				field: &s.record,
				onStarted: func() {
					startedAt = time.Now()
				},
				onStopped: func() {
					if err := injectBWFTimeReference(outputPath, startedAt, mic.SampleRate); err != nil {
						logger.Error("[%s] BWF timecode injection failed: %v", label, err)
					}
				},
			})
		}

		if doStream && !s.IsStreamRunning() {
			// videoOnly left false: intra-refresh is a hardware H.264/AV1
			// encoder feature, meaningless for the software Opus encoder.
			// isStream is true — WatchLocalStats' ABR loop applies here too.
			consEntries = append(consEntries, entry{
				label:       label + ":stream",
				str:         BuildAudioStreamStr(mic, srtPort),
				slot:        s,
				field:       &s.stream,
				isStream:    true,
				encoderName: "aenc",
				maxBitrate:  mic.StreamBitrate(),
			})
		}
	}

	startEntries(consEntries)
}
