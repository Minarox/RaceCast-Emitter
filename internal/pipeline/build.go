package pipeline

import (
	"fmt"
	"strings"

	"racecast-emitter/internal/config"
)

// flipMethod retourne l'index flip-method nvvidconv.
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

// BuildVideoStr construit la description du pipeline GStreamer pour une caméra.
//
//   - outputPath=="" → diffusion AV1 uniquement (appsink "sink")
//   - doStream=false → enregistrement H.264 MP4 uniquement (filesink)
//   - les deux       → tee : H.264 → filesink  +  AV1 → appsink "sink"
//
// Tous les chemins utilisent les encodeurs matériels Jetson Orin NX.
// L'appsink est présent uniquement quand doStream=true.
func BuildVideoStr(cam config.Camera, dev, outputPath string, doStream bool) string {
	flip := flipMethod(cam.VerticalFlip, cam.HorizontalFlip)

	// Chaîne source commune : v4l2 → décodage → flip → NVMM NV12
	var source string
	switch strings.ToUpper(cam.Format) {
	case "YUY2", "YUYV":
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv flip-method=%d ! "+
				"video/x-raw(memory:NVMM),format=NV12",
			dev, cam.Width, cam.Height, cam.Framerate, flip,
		)
	default: // MJPEG
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"image/jpeg,width=%d,height=%d,framerate=%d/1 ! "+
				"nvv4l2decoder mjpeg=true enable-max-performance=true ! "+
				"nvvidconv flip-method=%d ! "+
				"video/x-raw,format=NV12 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv ! "+
				"video/x-raw(memory:NVMM),format=NV12",
			dev, cam.Width, cam.Height, cam.Framerate, flip,
		)
	}

	// Branche d'enregistrement H.264 → MP4 fragmenté
	recordBranch := func() string {
		return fmt.Sprintf(
			"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
				"h264parse ! mp4mux fragment-duration=500 ! "+
				"filesink location=%s sync=false",
			videoBitrate(), cam.Framerate/2, outputPath,
		)
	}

	// Branche de diffusion AV1 → appsink (pas de réseau, consommé en CGo)
	streamBranch := func() string {
		return fmt.Sprintf(
			"nvvidconv ! "+
				"video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12 ! "+
				"nvv4l2av1enc bitrate=%d idrinterval=%d insert-seq-hdr=true ! "+
				"appsink name=sink max-buffers=2 drop=true sync=false",
			cam.StreamWidth(), cam.StreamHeight(), cam.StreamFramerate(),
			cam.StreamBitrate(), cam.StreamFramerate()/2,
		)
	}

	switch {
	case outputPath != "" && !doStream:
		return source + " ! " + recordBranch()

	case outputPath == "" && doStream:
		return source + " ! " + streamBranch()

	default: // enregistrement + diffusion simultanés via tee
		return source + " ! tee name=t " +
			// Branche enregistrement : queue sans leaky — on ne perd pas de frames
			"t. ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 ! " + recordBranch() + " " +
			// Branche diffusion : leaky=downstream — on préfère abandonner des frames
			// plutôt que bloquer la capture si l'encodeur AV1 est momentanément lent
			"t. ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 leaky=downstream ! " + streamBranch()
	}
}

// BuildAudioStr construit la description du pipeline GStreamer pour un microphone.
//
//   - outputPath=="" → diffusion Opus uniquement (appsink "sink")
//   - doStream=false → enregistrement AAC MP4 uniquement (avec piste vidéo noire SMPTE)
//   - les deux       → tee : AAC → MP4  +  Opus → appsink "sink"
func BuildAudioStr(mic config.Microphone, alsaDev, outputPath string, doStream bool) string {
	const (
		blackFramerate = 25
		blackBitrate   = 100_000
	)

	// Source audio commune
	source := fmt.Sprintf(
		"alsasrc device=%s do-timestamp=true ! "+
			"audio/x-raw,rate=%d,channels=%d ! "+
			"audioconvert",
		alsaDev, mic.SampleRate, mic.Channels,
	)

	// Piste vidéo noire pour le timecode SMPTE (enregistrement uniquement)
	blackTrack := func() string {
		return fmt.Sprintf(
			"videotestsrc pattern=black is-live=true ! "+
				"video/x-raw,width=320,height=240,framerate=%d/1 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! "+
				"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
				"h264parse ! mux.",
			blackFramerate, blackBitrate, blackFramerate/2,
		)
	}

	// Muxer MP4 + filesink (déclaré en premier car les branches "mux." le référencent)
	muxSink := func() string {
		return fmt.Sprintf(
			"mp4mux name=mux fragment-duration=500 ! filesink location=%s sync=false",
			outputPath,
		)
	}

	// Branche Opus → appsink
	opusBranch := func() string {
		return fmt.Sprintf(
			"opusenc bitrate=%d frame-size=20 perfect-timestamp=true ! "+
				"appsink name=sink max-buffers=8 drop=true sync=false",
			mic.StreamBitrate(),
		)
	}

	// Branche AAC → mux
	aacBranch := func() string {
		return fmt.Sprintf(
			"audioconvert ! avenc_aac bitrate=%d ! aacparse ! mux.",
			audioBitrate(),
		)
	}

	switch {
	case outputPath != "" && !doStream:
		// Enregistrement seul : muxer + piste vidéo noire + audio AAC
		return muxSink() + " " +
			blackTrack() + " " +
			source + " ! " + aacBranch()

	case outputPath == "" && doStream:
		// Diffusion seule : pas de piste vidéo noire, pas de muxer
		return source + " ! " + opusBranch()

	default: // enregistrement + diffusion via tee
		// Branche d'enregistrement avec audioconvert dédié pour négociation indépendante
		return muxSink() + " " +
			blackTrack() + " " +
			source + " ! tee name=at " +
			"at. ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 ! " + aacBranch() + " " +
			"at. ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 leaky=downstream ! " + opusBranch()
	}
}
