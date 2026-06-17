package pipeline

import (
	"context"
	"sync"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	lkproto "github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/logger"
)

// publishVideoTrack crée un track vidéo AV1 dans la room LiveKit et démarre
// une goroutine qui transfère les frames depuis l'appsink GStreamer vers LiveKit.
// La packetisation RTP AV1 est gérée par le SDK LiveKit/pion.
//
// La goroutine vit jusqu'à l'annulation de ctx. Quand le pipeline GStreamer
// redémarre après une reconnexion (nouveau canal frames), la goroutine reprend
// automatiquement sans republier le track.
func publishVideoTrack(ctx context.Context, cam config.Camera, gp *GstPipeline, room *lksdk.Room, wg *sync.WaitGroup) error {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeAV1,
		ClockRate: 90000,
	})
	if err != nil {
		return err
	}

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   cam.Name,
		Source: lkproto.TrackSource_CAMERA,
	}); err != nil {
		return err
	}

	logger.Info("[stream:%s] Track vidéo AV1 publié dans LiveKit", cam.Name)
	forwardFrames(ctx, gp, func(f Frame) error {
		return track.WriteSample(media.Sample{Data: f.Data, Duration: f.Duration}, nil)
	}, cam.Name, wg, func() {
		logger.Info("[stream:%s] Track vidéo AV1 retiré de LiveKit", cam.Name)
	})
	return nil
}

// publishAudioTrack crée un track audio Opus dans la room LiveKit.
// Même fonctionnement que publishVideoTrack.
func publishAudioTrack(ctx context.Context, mic config.Microphone, gp *GstPipeline, room *lksdk.Room, wg *sync.WaitGroup) error {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		return err
	}

	if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   mic.Name,
		Source: lkproto.TrackSource_MICROPHONE,
		Stereo: true,
	}); err != nil {
		return err
	}

	logger.Info("[stream:%s] Track audio Opus publié dans LiveKit", mic.Name)
	forwardFrames(ctx, gp, func(f Frame) error {
		return track.WriteSample(media.Sample{Data: f.Data, Duration: f.Duration}, nil)
	}, mic.Name, wg, func() {
		logger.Info("[stream:%s] Track audio Opus retiré de LiveKit", mic.Name)
	})
	return nil
}

// forwardFrames démarre une goroutine qui lit les frames depuis gp et les passe
// à write. Quand le canal frames est fermé (pipeline arrêté), la goroutine attend
// que gp soit remplacé (Poll le fera) puis recommence.
// onStop est appelé une seule fois à l'arrêt définitif de la goroutine (ctx annulé).
func forwardFrames(ctx context.Context, gp *GstPipeline, write func(Frame) error, label string, wg *sync.WaitGroup, onStop func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer onStop()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			ch := gp.Frames()
			if ch == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-ch:
					if !ok {
						goto waitRestart
					}
					if err := write(f); err != nil {
						logger.Warn("[stream:%s] WriteSample : %v", label, err)
					}
				}
			}
		waitRestart:
			// Pause courte avant que Poll relance un nouveau pipeline
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
}

