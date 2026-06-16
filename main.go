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

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/env"
	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/pipeline"
	"racecast-emitter/internal/udev"
	"racecast-emitter/internal/ups"
)

const configFile = "devices.yaml"

func main() {
	env.Load(".env")
	upsFlag := flag.Bool("ups", false, "Lit et affiche les valeurs de l'UPS en continu (optionnel : intervalle en secondes, défaut 5)")
	flag.Parse()

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

	closeLog := logger.Init()
	defer closeLog()

	cfg, err := config.Load(configFile)
	if err != nil {
		logger.Fatal("Erreur de configuration : %v", err)
	}

	// Création des slots (un par périphérique configuré)
	cameraSlots := make(map[string]*pipeline.Slot, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if cam.Disabled {
			logger.Info("[camera:%s] Désactivée dans la configuration, ignorée", cam.Name)
			continue
		}
		cameraSlots[cam.UID] = &pipeline.Slot{}
	}
	micSlots := make(map[string]*pipeline.Slot, len(cfg.Microphones))
	for _, mic := range cfg.Microphones {
		micSlots[mic.UID] = &pipeline.Slot{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Écoute des événements udev (connexion/déconnexion USB via netlink)
	events, err := udev.Listen(ctx)
	if err != nil {
		logger.Fatal("Impossible d'ouvrir le socket netlink udev : %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		// Scan initial pour les périphériques déjà connectés au démarrage
		pipeline.Poll(ctx, cfg, cameraSlots, micSlots, &wg)

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
				// Boucle de retry avec délais croissants : le nœud /dev peut
				// mettre un temps variable à apparaître après l'événement udev.
				// Poll est idempotent (ne redémarre pas un pipeline déjà actif),
				// donc le rappeler plusieurs fois est sans effet de bord.
				// Fenêtre totale : ~5s (300+500+1000+2000+1200ms de délais cumulés).
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
					pipeline.Poll(ctx, cfg, cameraSlots, micSlots, &wg)
				}
			}
		}
	}()

	// Attente d'un signal d'arrêt (Ctrl+C, SIGTERM, etc.)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("Signal %s reçu — envoi de l'EOS aux pipelines...", sig)

	// Annuler le contexte en premier pour bloquer tout nouveau démarrage
	cancel()

	// Envoyer EOS à tous les pipelines actifs
	for _, s := range cameraSlots {
		s.SendEOS()
	}
	for _, s := range micSlots {
		s.SendEOS()
	}

	wg.Wait()
	logger.Info("Tous les pipelines sont arrêtés. Enregistrement terminé.")
}
