package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/devices"
	"racecast-emitter/internal/logger"
)

const recordsDir = "records"

// envInt lit une variable d'environnement et la convertit en int.
// Retourne defaultVal si la variable est absente ou invalide.
func envInt(key string, defaultVal int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
		logger.Warn("Variable d'environnement %s invalide, valeur par défaut utilisée : %d", key, defaultVal)
	}
	return defaultVal
}

func videoBitrate() int { return envInt("RC_VIDEO_BITRATE", 12_000_000) }
func audioBitrate() int { return envInt("RC_AUDIO_BITRATE", 192_000) }

// ---------------------------------------------------------------------------
// Construction des pipelines GStreamer
// ---------------------------------------------------------------------------

// flipMethod convertit les flags vertical/horizontal en index flip-method nvvidconv.
// 0=aucun, 2=rotate-180, 4=miroir horizontal, 6=miroir vertical
func flipMethod(vertical, horizontal bool) int {
	switch {
	case vertical && horizontal:
		return 2
	case horizontal:
		return 4
	case vertical:
		return 6
	default:
		return 0
	}
}

func sanitize(name string) string {
	return strings.NewReplacer(" ", "_", "/", "-").Replace(name)
}

// BuildVideo construit les arguments gst-launch-1.0 pour une caméra.
// Utilise l'encodeur matériel nvv4l2h264enc via nvvidconv (Jetson Orin NX).
func BuildVideo(cam config.Camera, dev, outputPath string) []string {
	flip := flipMethod(cam.VerticalFlip, cam.HorizontalFlip)
	framerate := fmt.Sprintf("%d/1", cam.Framerate)

	var args []string

	switch strings.ToUpper(cam.Format) {
	case "YUY2", "YUYV":
		// Buffers CPU depuis v4l2src : timecodestamper en premier (n'accède pas aux pixels),
		// puis nvvidconv transfère en NVMM + applique le flip en un seul appel VIC.
		args = []string{
			"-e", "v4l2src", "device=" + dev,
			"!", fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%s", cam.Width, cam.Height, framerate),
			"!", "timecodestamper", "source=rtc",
			"!", "nvvidconv", fmt.Sprintf("flip-method=%d", flip),
			"!", "video/x-raw(memory:NVMM),format=NV12",
		}
	default:
		// MJPEG : décodage matériel (sortie NVMM).
		// Premier nvvidconv (VIC) : NVMM→CPU + flip en un seul passage.
		// timecodestamper annote les métadonnées du buffer sans toucher aux pixels.
		// Second nvvidconv (VIC) : CPU→NVMM pour l'encodeur.
		args = []string{
			"-e", "v4l2src", "device=" + dev,
			"!", fmt.Sprintf("image/jpeg,width=%d,height=%d,framerate=%s", cam.Width, cam.Height, framerate),
			"!", "nvv4l2decoder", "mjpeg=true", "enable-max-performance=true",
			"!", "nvvidconv", fmt.Sprintf("flip-method=%d", flip),
			"!", "video/x-raw,format=NV12",
			"!", "timecodestamper", "source=rtc",
			"!", "nvvidconv",
			"!", "video/x-raw(memory:NVMM),format=NV12",
		}
	}

	return append(args,
		// Encodage H.264 matériel — paramètres optimisés pour la Jetson Orin NX
		"!", "nvv4l2h264enc",
		fmt.Sprintf("bitrate=%d", videoBitrate()),
		fmt.Sprintf("idrinterval=%d", cam.Framerate/2),
		"insert-sps-pps=true", // SPS/PPS embarqués à chaque IDR : résistance aux coupures et seek fiable
		"profile=4",           // High profile : meilleure compression à débit équivalent
		// MP4 fragmenté : chaque fragment de 500ms est autonome, résistant aux coupures de courant
		"!", "h264parse",
		"!", "mp4mux", "fragment-duration=500",
		"!", "filesink", "location="+outputPath, "sync=false",
	)
}

// BuildAudio construit les arguments gst-launch-1.0 pour un microphone.
// Le fichier produit est un .mp4 fragmenté contenant :
//   - une piste vidéo 320x240 noire avec timecode SMPTE wall-clock (pour l'alignement DaVinci)
//   - une piste audio AAC 192 kbps
//
// La piste vidéo noire est négligeable en charge : l'encodeur H.264 ne produit
// que des skip-macroblocks après l'IDR, le bitrate effectif est <10 kbps.
func BuildAudio(mic config.Microphone, alsaDev, outputPath string) []string {
	const (
		blackFramerate = 25
		blackBitrate   = 100_000 // 100 kbps, largement suffisant pour du noir
	)
	return []string{
		"-e",
		// Muxer nommé défini en premier — les deux branches l'alimentent
		// MP4 fragmenté : chaque fragment de 500ms est autonome, résistant aux coupures de courant
		"mp4mux", "name=mux", "fragment-duration=500",
		"!", "filesink", "location=" + outputPath, "sync=false",
		// Branche vidéo : flux noir 320x240 avec timecode SMPTE wall-clock
		"videotestsrc", "pattern=black",
		"!", fmt.Sprintf("video/x-raw,width=320,height=240,framerate=%d/1", blackFramerate),
		"!", "timecodestamper", "source=rtc",
		"!", "nvvidconv",
		"!", "video/x-raw(memory:NVMM),format=NV12",
		"!", "nvv4l2h264enc",
		fmt.Sprintf("bitrate=%d", blackBitrate),
		fmt.Sprintf("idrinterval=%d", blackFramerate/2),
		"insert-sps-pps=true",
		"profile=4",
		"!", "h264parse",
		"!", "mux.",
		// Branche audio
		"alsasrc", "device=" + alsaDev,
		"!", fmt.Sprintf("audio/x-raw,rate=%d,channels=%d", mic.SampleRate, mic.Channels),
		"!", "audioconvert",
		"!", "avenc_aac", fmt.Sprintf("bitrate=%d", audioBitrate()),
		"!", "aacparse",
		"!", "mux.",
	}
}

// ---------------------------------------------------------------------------
// Gestion dynamique des pipelines
// ---------------------------------------------------------------------------

// Slot représente l'emplacement d'un pipeline pour un périphérique donné.
// Un seul pipeline peut tourner à la fois par slot.
type Slot struct {
	mu       sync.Mutex
	cmd      *exec.Cmd // nil = aucun pipeline en cours
	notFound bool      // true si l'absence a déjà été loggée (évite le spam)
}

// IsRunning indique si un pipeline est actuellement actif.
func (s *Slot) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// Start lance un nouveau pipeline dans ce slot si aucun n'est déjà en cours
// et que le contexte n'est pas annulé.
func (s *Slot) Start(ctx context.Context, label string, args []string, wg *sync.WaitGroup) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ctx.Err() != nil || s.cmd != nil {
		return
	}

	cmd := exec.Command("gst-launch-1.0", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Processus dans son propre groupe de processus : le SIGINT du terminal
	// n'est pas propagé directement, laissant le programme gérer l'arrêt proprement.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logger.Error("[%s] Échec du démarrage : %v", label, err)
		return
	}
	s.cmd = cmd
	logger.Info("[%s] Pipeline démarré (pid %d)", label, cmd.Process.Pid)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := cmd.Wait()

		s.mu.Lock()
		s.cmd = nil
		s.mu.Unlock()

		if err == nil {
			logger.Info("[%s] Arrêté proprement", label)
			return
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() != 0 {
			logger.Warn("[%s] Terminé avec le code %d (déconnexion ?)", label, exitErr.ExitCode())
		}
	}()
}

// SendEOS envoie SIGINT au groupe de processus du pipeline actif, ce qui
// pousse gst-launch-1.0 -e à déclencher un EOS et à finaliser le fichier.
func (s *Slot) SendEOS() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
			logger.Error("SendEOS : impossible d'envoyer SIGINT au groupe %d : %v", cmd.Process.Pid, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Boucle de surveillance des périphériques
// ---------------------------------------------------------------------------

// Poll parcourt tous les périphériques configurés et démarre un pipeline
// pour chacun d'eux si présent et non encore actif.
func Poll(ctx context.Context, cfg *config.Config, cameraSlots, micSlots map[string]*Slot, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}

	for i := range cfg.Cameras {
		cam := cfg.Cameras[i]
		if cam.Disabled {
			continue
		}
		s, ok := cameraSlots[cam.UID]
		if !ok || s.IsRunning() {
			continue
		}
		dev, err := devices.FindVideo(cam.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[camera:%s] Introuvable (UID: %s), en attente de connexion...", cam.Name, cam.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false // périphérique retrouvé, réinitialisation du flag
		now := time.Now()
		outputDir := filepath.Join(recordsDir, now.Format("2006-01-02"))
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			logger.Error("[camera:%s] Impossible de créer le répertoire : %v", cam.Name, err)
			continue
		}
		name := sanitize(cam.Name)
		outputPath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_video.mp4", now.Format("15-04-05"), name))
		label := "camera:" + name
		logger.Info("[%s] Périphérique détecté : %s → %s", label, dev, outputPath)
		s.Start(ctx, label, BuildVideo(cam, dev, outputPath), wg)
	}

	for i := range cfg.Microphones {
		mic := cfg.Microphones[i]
		s, ok := micSlots[mic.UID]
		if !ok || s.IsRunning() {
			continue
		}
		alsaDev, err := devices.FindALSA(mic.UID)
		if err != nil {
			if !s.notFound {
				logger.Warn("[mic:%s] Introuvable (UID: %s), en attente de connexion...", mic.Name, mic.UID)
				s.notFound = true
			}
			continue
		}
		s.notFound = false // périphérique retrouvé, réinitialisation du flag
		now := time.Now()
		outputDir := filepath.Join(recordsDir, now.Format("2006-01-02"))
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			logger.Error("[mic:%s] Impossible de créer le répertoire : %v", mic.Name, err)
			continue
		}
		name := sanitize(mic.Name)
		outputPath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_audio.mp4", now.Format("15-04-05"), name))
		label := "mic:" + name
		logger.Info("[%s] Périphérique détecté : %s → %s", label, alsaDev, outputPath)
		s.Start(ctx, label, BuildAudio(mic, alsaDev, outputPath), wg)
	}
}
