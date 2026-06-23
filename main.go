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
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		modem.Run(ctx)
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
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ups.Run(ctx, interval)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup

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

		// Connectivity watcher: close the stream valve on all pipelines when the
		// modem loses internet, reopen and force an IDR when it reconnects.
		wg.Add(1)
		go func() {
			defer wg.Done()
			modem.WatchConnectivity(ctx, func(connected bool) {
				for _, s := range cameraSlots {
					s.SetStreamValve(!connected)
				}
				for _, s := range micSlots {
					s.SetStreamValve(!connected)
				}
				if connected {
					logger.Info("[stream] Modem connected — stream valve opened")
				} else {
					logger.Info("[stream] Modem disconnected — stream valve closed")
				}
			})
		}()
	}

	events, err := udev.Listen(ctx)
	if err != nil {
		logger.Fatal("Failed to open netlink udev socket: %v", err)
	}

	pollOpts := pipeline.PollOptions{Record: doRecord, Stream: doStream}

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

	<-ctx.Done()
	logger.Info("Signal received -- stopping pipelines...")

	// Restore default signal handling: a second Ctrl+C kills immediately.
	stop()

	for _, s := range cameraSlots {
		s.SendEOS()
	}
	for _, s := range micSlots {
		s.SendEOS()
	}

	// Safety: if wg.Wait() doesn't return within 5 s (e.g. StartAll blocked
	// on gst_element_get_state), force exit. The kernel releases all resources.
	time.AfterFunc(5*time.Second, func() {
		logger.Warn("Shutdown timeout exceeded — forcing exit")
		os.Exit(0)
	})

	wg.Wait()
	logger.Info("All pipelines stopped.")
}
