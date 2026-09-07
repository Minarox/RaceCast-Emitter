package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/dashboard"
	"racecast-emitter/internal/env"
	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/modem"
	"racecast-emitter/internal/pipeline"
	"racecast-emitter/internal/telemetry"
	"racecast-emitter/internal/udev"
	"racecast-emitter/internal/ups"
)

const configFile = "devices.yaml"

func main() {
	startTime := time.Now()
	env.Load(".env")
	debugModemFlag := flag.Bool("debug-modem", false, "Only log modem events to the daily log file for debugging (kernel USB faults, ModemManager state changes, periodic signal/GPS snapshot) — no recording, streaming, or telemetry")
	recordFlag := flag.Bool("record", false, "Record only (no SRT streaming)")
	streamFlag := flag.Bool("stream", false, "Stream only (no recording)")
	fakeDevicesFlag := flag.Bool("fake-devices", false, "Replace every camera/microphone with a synthetic source (snow test pattern + white noise — real generated data, not silence) instead of capturing from real hardware, to test recording/streaming/SRT transmission without cameras or mics attached")
	flag.Parse()

	if *debugModemFlag && (*recordFlag || *streamFlag) {
		logger.Fatal("[main] --debug-modem, --record and --stream are mutually exclusive")
	}
	if *debugModemFlag && *fakeDevicesFlag {
		logger.Fatal("[main] --debug-modem does not touch cameras/microphones — --fake-devices has no effect with it")
	}

	if *debugModemFlag {
		closeLog := logger.Init()
		defer closeLog()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
			<-sigCh
			logger.Warn("[main] Second signal — forcing exit")
			os.Exit(0)
		}()
		modem.RunDiagnostics(ctx)
		signal.Stop(sigCh)
		return
	}

	doRecord := !*streamFlag
	doStream := !*recordFlag

	closeLog := logger.Init()
	defer closeLog()

	cfg, err := config.Load(configFile)
	if err != nil {
		logger.Fatal("[main] Configuration error: %v", err)
	}

	if *fakeDevicesFlag {
		logger.Warn("[main] --fake-devices active — every camera/microphone is a synthetic snow/white-noise source, NOT real hardware")
	}

	cameraSlots := make(map[string]*pipeline.Slot, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if cam.Disabled {
			logger.Info("[camera:%s] Disabled in config, skipping", cam.Name)
			continue
		}
		if doRecord || cam.HasStream() {
			cameraSlots[cam.UID] = &pipeline.Slot{}
		}
	}
	micSlots := make(map[string]*pipeline.Slot, len(cfg.Microphones))
	for _, mic := range cfg.Microphones {
		if mic.Disabled {
			logger.Info("[mic:%s] Disabled in config, skipping", mic.Name)
			continue
		}
		if doRecord || mic.HasStream() {
			micSlots[mic.UID] = &pipeline.Slot{}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	// Buffer 2 so the second signal is never dropped even if the first is
	// processed synchronously.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if doStream && os.Getenv("RC_SRT_HOST") == "" {
		logger.Warn("[srt] RC_SRT_HOST not set -- SRT streaming will be inactive")
	}

	// Build camera name → slot map for IDR dispatch via the telemetry connection.
	cameraByName := make(map[string]*pipeline.Slot, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if s, ok := cameraSlots[cam.UID]; ok {
			cameraByName[cam.Name] = s
		}
	}

	telemConn := telemetry.NewConn(ctx, "telemetry", func(msg []byte) {
		var req struct {
			Type   string `json:"type"`
			Camera string `json:"camera"`
		}
		if json.Unmarshal(msg, &req) != nil || req.Type != "idr" {
			return
		}
		if s, ok := cameraByName[req.Camera]; ok {
			s.ForceIDR("avenc")
		}
	})
	defer telemConn.Close()

	// Dedicated side-channel for video capture timestamps (streamid
	// "frametime") — deliberately a separate Conn from telemConn above, not
	// reused: see PollOptions.FrameTimeConn's comment in internal/pipeline
	// for why. One-way (onRecv nil): the receiver never sends anything back
	// over this connection.
	frameTimeConn := telemetry.NewConn(ctx, "frametime", nil)
	defer frameTimeConn.Close()

	// UPS/GPS telemetry: read and recorded locally (records/<date>/data/) any
	// time recording is on, and additionally sent to the receiver over SRT
	// when streaming is on too — mirrors video/audio, where local recording
	// never depends on streaming being active. doStream is passed through so
	// each RunStream only calls Conn.Send when streaming is actually enabled;
	// --record-only must never touch the network even if RC_SRT_HOST happens
	// to be set, matching that flag's documented "no SRT streaming" contract.
	if doRecord || doStream {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ups.RunStream(ctx, telemConn, doStream)
		}()

		// Single shared poller for ModemManager's signal-quality/tech D-Bus
		// properties: RunStream, WatchConnectivity, WatchHealth and the
		// per-camera ABR loop (feedback.go) all read modem.CachedSignalStats
		// instead of polling D-Bus themselves — no ordering requirement, the
		// cache just reads as "not fresh yet" until this goroutine's first tick.
		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.WatchSignalStats(ctx)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.RunStream(ctx, telemConn, doStream)
		}()
	}

	if doStream {
		// Kernel-log watcher: surfaces USB power/enumeration fault signatures
		// (undervoltage, over-current, disconnect, reset) for the modem's own
		// bus path in the app's own log, for live debugging during a race.
		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.WatchKernelLog(ctx)
		}()

		// Health watchdog: detects a stuck cellular data bearer (radio
		// registered per ModemManager, but no traffic actually gets through)
		// and recovers via a reconnect nudge, then a full modem reset if that
		// doesn't help. WatchConnectivity below reflects its "recovering"
		// state so streaming gets paused for the duration of a recovery
		// attempt rather than treating the radio-only signal as gospel.
		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.WatchHealth(ctx)
		}()

		// Connectivity watcher: pause the stream pipeline on all slots when the
		// modem loses internet, resume and force an IDR when it reconnects.
		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.WatchConnectivity(ctx, func(connected bool) {
				for _, s := range cameraSlots {
					if connected {
						s.ResumeStream()
					} else {
						s.PauseStream()
					}
				}
				for _, s := range micSlots {
					if connected {
						s.ResumeStream()
					} else {
						s.PauseStream()
					}
				}
				if connected {
					logger.Info("[stream] Modem connected — stream resumed")
				} else {
					logger.Info("[stream] Modem disconnected — stream paused")
				}
			})
		}()
	}

	events, err := udev.Listen(ctx)
	if err != nil {
		logger.Fatal("[udev] Failed to open netlink udev socket: %v", err)
	}

	// Full-screen console status view, replacing scrolling log output for the
	// rest of this run (the JSON log file is unaffected). Works for record-only
	// and stream-only modes too — the relevant lines just report "disabled".
	// Started only once init can no longer logger.Fatal (which os.Exit(1)s
	// without running deferred cleanup — the dashboard's own defer needs to
	// run to leave the terminal in a sane state).
	wg.Add(1)
	go func() {
		defer wg.Done()
		dashboard.Run(ctx, dashboard.Deps{
			StartTime:   startTime,
			Cfg:         cfg,
			CameraSlots: cameraSlots,
			MicSlots:    micSlots,
			TelemConn:   telemConn,
			DoRecord:    doRecord,
			DoStream:    doStream,
		})
	}()

	pollOpts := pipeline.PollOptions{Record: doRecord, Stream: doStream, FakeDevices: *fakeDevicesFlag}
	if doStream {
		pollOpts.NotifyClose = func(name string) { _ = telemConn.SendStreamClose(name) }
		pollOpts.FrameTimeConn = frameTimeConn

		// Shared video bandwidth budget across every streaming camera — see
		// bandwidth.go. Only meaningful while actually streaming; no reason to
		// run its allocation loop in --record-only mode.
		bwCoord := pipeline.NewBandwidthCoordinator(ctx)
		pollOpts.Bandwidth = bwCoord
		wg.Add(1)
		go func() {
			defer wg.Done()
			bwCoord.Run()
		}()
	}

	// The control goroutine is NOT tracked in wg (only pipeline goroutines are).
	// This prevents wg.Wait() from blocking on StartAll's CGo get_state call.
	go func() {
		pipeline.Poll(ctx, pollOpts, cfg, cameraSlots, micSlots, &wg)

		// Fallback poll, independent of udev: a pipeline that errors out for a
		// reason other than a physical unplug (encoder hiccup, SRT/GStreamer
		// bus error, transient NVMM failure under thermal stress) never gets a
		// udev "add" event, so without this ticker the affected slot would stay
		// down for the rest of the run. Poll() is a cheap no-op for every slot
		// that's already running, so a slow interval here just bounds the
		// worst-case time-to-recovery for this class of failure.
		fallback := time.NewTicker(15 * time.Second)
		defer fallback.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-fallback.C:
				pipeline.Poll(ctx, pollOpts, cfg, cameraSlots, micSlots, &wg)
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Action != "add" {
					continue
				}
				for _, delay := range []time.Duration{
					300 * time.Millisecond,
					500 * time.Millisecond,
					1 * time.Second,
					2 * time.Second,
				} {
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return
					}
					pipeline.Poll(ctx, pollOpts, cfg, cameraSlots, micSlots, &wg)
				}
			}
		}
	}()

	// ── Shutdown ─────────────────────────────────────────────────────────────
	// Wait for the first Ctrl+C / SIGTERM.
	<-sigCh
	logger.Info("[main] Signal received — stopping pipelines...")

	// Cancel the main context: stops the control goroutine, udev listener,
	// telemetry, ups/modem streams, and prevents new pipelines from starting.
	cancel()

	// Second signal → force-exit immediately (no matter the state of cleanup).
	go func() {
		<-sigCh
		logger.Warn("[main] Second signal received — forcing exit")
		os.Exit(1)
	}()

	// Notify the receiver before tearing down pipelines so it can immediately
	// unpublish LiveKit tracks without waiting for the reconnect grace period.
	if doStream {
		for _, cam := range cfg.Cameras {
			if s, ok := cameraSlots[cam.UID]; ok && s.IsStreamRunning() {
				_ = telemConn.SendStreamClose(pipeline.StreamKey(cam.Name, "camera"))
			}
		}
		for _, mic := range cfg.Microphones {
			if s, ok := micSlots[mic.UID]; ok && s.IsStreamRunning() {
				_ = telemConn.SendStreamClose(pipeline.StreamKey(mic.Name, "microphone"))
			}
		}
	}

	// Stop all running pipelines: source and stream are cancelled immediately;
	// record receives EOS so that mp4mux flushes and filesink closes cleanly.
	for _, s := range cameraSlots {
		s.Stop()
	}
	for _, s := range micSlots {
		s.Stop()
	}

	logger.Info("[main] Waiting for pipelines to stop... (Ctrl+C again to force)")
	wg.Wait()
	logger.Info("[main] All pipelines stopped.")
}
