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

	lksdk "github.com/livekit/server-sdk-go/v2"

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
		logger.Warn("Variable d'environnement %s invalide, valeur par defaut : %d", key, defaultVal)
	}
	return defaultVal
}

func videoBitrate() int { return envInt("RC_VIDEO_BITRATE", 12_000_000) }
func audioBitrate() int { return envInt("RC_AUDIO_BITRATE", 192_000) }

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

func (s *Slot) Gst() *GstPipeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gst
}

func (s *Slot) start(ctx context.Context, label, pipelineStr string, hasAppsink bool, wg *sync.WaitGroup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.gst != nil {
		return
	}
	gp, err := newGstPipeline(pipelineStr, hasAppsink)
	if err != nil {
		logger.Error("[%s] Impossible de créer le pipeline : %v", label, err)
		return
	}
	gp.SetOnError(func() {
		s.mu.Lock()
		if s.gst == gp {
			gp.SetNull()
			gp.Free()
			s.gst = nil
		}
		s.mu.Unlock()
		logger.Warn("[%s] Pipeline en erreur -- périphérique déconnecté ?", label)
	})
	if err := gp.Start(); err != nil {
		gp.Free()
		logger.Error("[%s] Impossible de démarrer le pipeline : %v", label, err)
		return
	}
	s.activate(label, gp, wg)
}

// activate enregistre un GstPipeline déjà démarré dans le slot et lance la goroutine
// de surveillance qui appelle SetNull/Free une fois que le pipeline s'est arrêté.
func (s *Slot) activate(label string, gp *GstPipeline, wg *sync.WaitGroup) {
	s.mu.Lock()
	s.gst = gp
	s.mu.Unlock()
	logger.Info("[%s] Pipeline GStreamer démarré", label)
	wg.Add(1)
	go func() {
		defer wg.Done()
		gp.wg.Wait()
		// Logger avant SetNull/Free : ces fonctions silencient fd 1+2 pendant la
		// libération des ressources Nvidia ; si on loggait après, un autre pipeline
		// simultané en cours de SetNull/Free absorberait ce message dans /dev/null.
		logger.Info("[%s] Pipeline GStreamer arrêté", label)
		s.mu.Lock()
		if s.gst == gp {
			gp.SetNull()
			gp.Free()
			s.gst = nil
		}
		s.mu.Unlock()
	}()
}

func (s *Slot) SendEOS() {
	s.mu.Lock()
	gp := s.gst
	s.mu.Unlock()
	if gp != nil {
		gp.SendEOS()
	}
}

type PollOptions struct {
	Record bool
	Stream bool
	Room   *lksdk.Room
}

func Poll(ctx context.Context, opts PollOptions, cfg *config.Config, cameraSlots, micSlots map[string]*Slot, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	// startEntry regroupe tout ce qui est nécessaire pour démarrer un pipeline
	// et publier son track LiveKit une fois prêt.
	type startEntry struct {
		label       string
		pipelineStr string
		hasAppsink  bool
		slot        *Slot
		// publishTrack est appelé après que le pipeline est en PLAYING.
		// nil si la diffusion n'est pas activée pour ce périphérique.
		publishTrack func(gp *GstPipeline) error
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
		doStream := opts.Stream && opts.Room != nil && cam.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		dev, err := devices.FindVideo(cam.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[camera:%s] Introuvable (UID: %s), en attente...", cam.Name, cam.UID)
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
				logger.Error("[camera:%s] Impossible de créer le répertoire : %v", cam.Name, err)
				continue
			}
			outputPath = filepath.Join(dir, fmt.Sprintf("%s_%s_video.mp4", now.Format("15-04-05"), sanitize(cam.Name)))
		}
		label := "camera:" + sanitize(cam.Name)
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s -> enregistrement + diffusion AV1", label, dev)
		case opts.Record:
			logger.Info("[%s] %s -> enregistrement H.264", label, dev)
		default:
			logger.Info("[%s] %s -> diffusion AV1", label, dev)
		}
		var pub func(*GstPipeline) error
		if doStream {
			camCopy := cam
			pub = func(gp *GstPipeline) error {
				return publishVideoTrack(ctx, camCopy, gp, opts.Room, wg)
			}
		}
		entries = append(entries, startEntry{
			label:        label,
			pipelineStr:  BuildVideoStr(cam, dev, outputPath, doStream),
			hasAppsink:   doStream,
			slot:         s,
			publishTrack: pub,
		})
	}

	for i := range cfg.Microphones {
		mic := cfg.Microphones[i]
		s := micSlots[mic.UID]
		if s == nil || s.IsRunning() {
			continue
		}
		doStream := opts.Stream && opts.Room != nil && mic.HasStream()
		if !opts.Record && !doStream {
			continue
		}
		alsaDev, err := devices.FindALSA(mic.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[mic:%s] Introuvable (UID: %s), en attente...", mic.Name, mic.UID)
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
				logger.Error("[mic:%s] Impossible de créer le répertoire : %v", mic.Name, err)
				continue
			}
			outputPath = filepath.Join(dir, fmt.Sprintf("%s_%s_audio.mp4", now.Format("15-04-05"), sanitize(mic.Name)))
		}
		label := "mic:" + sanitize(mic.Name)
		switch {
		case opts.Record && doStream:
			logger.Info("[%s] %s -> enregistrement + diffusion Opus", label, alsaDev)
		case opts.Record:
			logger.Info("[%s] %s -> enregistrement AAC", label, alsaDev)
		default:
			logger.Info("[%s] %s -> diffusion Opus", label, alsaDev)
		}
		var pub func(*GstPipeline) error
		if doStream {
			micCopy := mic
			pub = func(gp *GstPipeline) error {
				return publishAudioTrack(ctx, micCopy, gp, opts.Room, wg)
			}
		}
		entries = append(entries, startEntry{
			label:        label,
			pipelineStr:  BuildAudioStr(mic, alsaDev, outputPath, doStream),
			hasAppsink:   doStream,
			slot:         s,
			publishTrack: pub,
		})
	}

	if len(entries) == 0 {
		return
	}

	// Phase 1 : créer tous les pipelines GStreamer (gst_parse_launch).
	// Cette étape est rapide et n'émet pas de messages Nvidia — pas de silencing requis.
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
		gp, err := newGstPipeline(e.pipelineStr, e.hasAppsink)
		if err != nil {
			logger.Error("[%s] Impossible de créer le pipeline : %v", e.label, err)
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
			logger.Warn("[%s] Pipeline en erreur -- périphérique déconnecté ?", e.label)
		})
		preps = append(preps, prepEntry{e, gp})
	}

	if len(preps) == 0 {
		return
	}

	// Phase 2 : démarrer tous les pipelines en parallèle sous un silencing global unique.
	// Chaque gst_element_get_state (bloquant ~1s) s'exécute dans sa propre goroutine ;
	// un seul dup2(/dev/null) couvre l'ensemble → pas de race sur fd 1.
	gpList := make([]*GstPipeline, len(preps))
	for i, pe := range preps {
		gpList[i] = pe.gp
	}
	errs := StartAll(gpList)

	// Phase 3 : activer les slots et publier les tracks LiveKit en parallèle.
	var pubWg sync.WaitGroup
	for i, pe := range preps {
		if errs[i] != nil {
			logger.Error("[%s] Impossible de démarrer le pipeline : %v", pe.label, errs[i])
			pe.gp.Free()
			continue
		}
		pe.slot.activate(pe.label, pe.gp, wg)
		if pe.publishTrack != nil {
			pe := pe
			pubWg.Add(1)
			go func() {
				defer pubWg.Done()
				if gp := pe.slot.Gst(); gp != nil {
					if err := pe.publishTrack(gp); err != nil {
						logger.Warn("[%s] Impossible de publier le track : %v", pe.label, err)
					}
				}
			}()
		}
	}
	pubWg.Wait()
}
