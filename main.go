package main

import (
	"context"
	"flag"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/env"
	"racecast-emitter/internal/livekit"
	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/pipeline"
	"racecast-emitter/internal/udev"
	"racecast-emitter/internal/ups"
)

const configFile = "devices.yaml"

func main() {
	env.Load(".env")
	upsFlag    := flag.Bool("ups",    false, "Lit et affiche les valeurs de l'UPS en continu (optionnel : intervalle en secondes, défaut 5)")
	recordFlag := flag.Bool("record", false, "Enregistrement uniquement (sans diffusion LiveKit)")
	streamFlag := flag.Bool("stream", false, "Diffusion LiveKit uniquement (sans enregistrement)")
	flag.Parse()

	nSet := 0
	for _, b := range []bool{*upsFlag, *recordFlag, *streamFlag} {
		if b {
			nSet++
		}
	}
	if nSet > 1 {
		logger.Fatal("Les options --ups, --record et --stream sont mutuellement exclusives")
	}

	if *upsFlag {
		logger.InitConsole()
		interval := 5 * time.Second
		if args := flag.Args(); len(args) > 0 {
			if n, err := strconv.ParseFloat(args[0], 64); err == nil && n >= 0.05 {
				ms := int64(math.Round(n * 1000))
				interval = time.Duration(ms) * time.Millisecond
			} else {
				logger.Fatal("[ups] Intervalle invalide : %q (nombre >= 0.05 attendu)", args[0])
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
		logger.Fatal("Erreur de configuration : %v", err)
	}

	cameraSlots := make(map[string]*pipeline.Slot, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if cam.Disabled {
			logger.Info("[camera:%s] Désactivée dans la configuration, ignorée", cam.Name)
			continue
		}
		if doRecord || cam.HasStream() {
			cameraSlots[cam.UID] = &pipeline.Slot{}
		}
	}
	micSlots := make(map[string]*pipeline.Slot, len(cfg.Microphones))
	for _, mic := range cfg.Microphones {
		if doRecord || mic.HasStream() {
			micSlots[mic.UID] = &pipeline.Slot{}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	var room *lksdk.Room
	if doStream {
		lkCfg, err := livekit.LoadConfig()
		if err != nil {
			logger.Warn("[livekit] Configuration incomplete, diffusion désactivée : %v", err)
		} else {
			r, err := livekit.Connect(lkCfg)
			if err != nil {
				logger.Warn("[livekit] Connexion échouée, diffusion désactivée : %v", err)
			} else {
				room = r
			}
		}
	}
	if room != nil {
		// Disconnect appelé explicitement après wg.Wait(), pas en defer,
		// pour que les tracks disparaissent de LiveKit après l'arrêt complet.
		defer func() {
			room.Disconnect()
			logger.Info("Déconnecté de LiveKit.")
		}()
	}

	events, err := udev.Listen(ctx)
	if err != nil {
		logger.Fatal("Impossible d'ouvrir le socket netlink udev : %v", err)
	}

	pollOpts := pipeline.PollOptions{Record: doRecord, Stream: doStream, Room: room}

	wg.Add(1)
	go func() {
		defer wg.Done()
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("Signal %s reçu -- arrêt des pipelines...", sig)

	// Annuler le contexte pour stopper les goroutines de polling et de diffusion.
	cancel()

	// Envoyer EOS à TOUS les pipelines actifs (enregistrement et diffusion).
	// Sans EOS, gp.wg.Wait() dans chaque slot ne se termine jamais et wg.Wait() bloque.
	for _, s := range cameraSlots {
		s.SendEOS()
	}
	for _, s := range micSlots {
		s.SendEOS()
	}

	wg.Wait()
	logger.Info("Tous les pipelines sont arrêtés.")
}
