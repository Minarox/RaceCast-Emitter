package main

import (
	"context"
	"encoding/json"
	"flag"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/env"
	"racecast-emitter/internal/modem"
	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/pipeline"
	"racecast-emitter/internal/telemetry"
	"racecast-emitter/internal/udev"
	"racecast-emitter/internal/ups"
)

const configFile = "devices.yaml"

func main() {
	env.Load(".env")
	upsFlag    := flag.Bool("ups",    false, "Continuously read and display UPS values (optional: interval in seconds, default 5)")
	modemFlag  := flag.Bool("modem",  false, "Continuously read and display modem data (GPS + network)")
	recordFlag := flag.Bool("record", false, "Record only (no SRT streaming)")
	streamFlag := flag.Bool("stream", false, "Stream only (no recording)")
	flag.Parse()

	nSet := 0
	for _, b := range []bool{*upsFlag, *modemFlag, *recordFlag, *streamFlag} {
		if b {
			nSet++
		}
	}
	if nSet > 1 {
		logger.Fatal("--ups, --modem, --record and --stream are mutually exclusive")
	}

	if *modemFlag {
		logger.InitConsole()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
			<-sigCh
			logger.Warn("Second signal — forcing exit")
			os.Exit(0)
		}()
		modem.Run(ctx)
		signal.Stop(sigCh)
		return
	}

	if *upsFlag {
		logger.InitConsole()
		interval := 5 * time.Second
		if args := flag.Args(); len(args) > 0 {
			if n, err := strconv.ParseFloat(args[0], 64); err == nil && n >= 0.05 {
				ms := int64(math.Round(n * 1000))
				interval = time.Duration(ms) * time.Millisecond
			} else {
				logger.Fatal("[ups] Invalid interval: %q (expected a number >= 0.05)", args[0])
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
			<-sigCh
			logger.Warn("Second signal — forcing exit")
			os.Exit(0)
		}()
		ups.Run(ctx, interval)
		signal.Stop(sigCh)
		return
	}

	doRecord := !*streamFlag
	doStream  := !*recordFlag

	closeLog := logger.Init()
	defer closeLog()

	cfg, err := config.Load(configFile)
	if err != nil {
		logger.Fatal("Configuration error: %v", err)
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

	// Send UPS values to the server every 2 s (when streaming is active).
	if doStream {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ups.RunStream(ctx, telemConn)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.RunStream(ctx, telemConn)
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
		logger.Fatal("Failed to open netlink udev socket: %v", err)
	}

	pollOpts := pipeline.PollOptions{Record: doRecord, Stream: doStream}
	if doStream {
		pollOpts.NotifyClose = func(name string) { _ = telemConn.SendStreamClose(name) }
	}

	// The control goroutine is NOT tracked in wg (only pipeline goroutines are).
	// This prevents wg.Wait() from blocking on StartAll's CGo get_state call.
	go func() {
		pipeline.Poll(ctx, pollOpts, cfg, cameraSlots, micSlots, &wg)
		for {
			select {
			case <-ctx.Done():
				return
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
	logger.Info("Signal received — stopping pipelines...")

	// Cancel the main context: stops the control goroutine, udev listener,
	// telemetry, ups/modem streams, and prevents new pipelines from starting.
	cancel()

	// Second signal → force-exit immediately (no matter the state of cleanup).
	go func() {
		<-sigCh
		logger.Warn("Second signal received — forcing exit")
		os.Exit(1)
	}()

	// Notify the receiver before tearing down pipelines so it can immediately
	// unpublish LiveKit tracks without waiting for the reconnect grace period.
	if doStream {
		for _, cam := range cfg.Cameras {
			if s, ok := cameraSlots[cam.UID]; ok && s.IsStreamRunning() {
				_ = telemConn.SendStreamClose(cam.Name)
			}
		}
		for _, mic := range cfg.Microphones {
			if s, ok := micSlots[mic.UID]; ok && s.IsStreamRunning() {
				_ = telemConn.SendStreamClose(mic.Name)
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

	logger.Info("Waiting for pipelines to stop... (Ctrl+C again to force)")
	wg.Wait()
	logger.Info("All pipelines stopped.")
}
