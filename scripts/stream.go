package scripts

import (
	"fmt"

	"racecast-emitter/utils"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

// AddVideoStream creates a GStreamer pipeline for the given camera config and
// publishes it to the LiveKit room.
// Returns the running pipeline, the LiveKit publication SID (for unpublishing),
// and any error. On error the caller does not need to clean up.
func AddVideoStream(room *lksdk.Room, cfg utils.VideoPipelineConfig, fakeStream bool) (*utils.GStreamerPipeline, string, error) {
	if fakeStream {
		utils.Log.Infow("Video stream: using SMPTE test pattern.")
	} else {
		utils.Log.Infow("Video stream: using webcam.", "device", cfg.Device)
	}

	pipeline, err := utils.NewVideoPipeline(cfg, fakeStream)
	if err != nil {
		return nil, "", fmt.Errorf("create video pipeline: %w", err)
	}

	track, pubSID, err := utils.PublishVideoTrack(room, cfg.Name)
	if err != nil {
		pipeline.Free()
		return nil, "", fmt.Errorf("publish video track: %w", err)
	}

	pipeline.AttachTrack(track)

	// Force an IDR frame as soon as each new subscriber binds to the track,
	// so viewers never wait more than one keyframe interval for the first image.
	track.OnBind(func() {
		pipeline.ForceKeyframe()
	})

	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		if unpubErr := room.LocalParticipant.UnpublishTrack(pubSID); unpubErr != nil {
			utils.Log.Warnw("Failed to unpublish track after pipeline start failure.", "error", unpubErr)
		}
		return nil, "", fmt.Errorf("start video pipeline: %w", err)
	}

	return pipeline, pubSID, nil
}

// AddAudioStream creates a GStreamer pipeline for the given microphone config
// and publishes it to the LiveKit room.
// Returns the running pipeline, the LiveKit publication SID (for unpublishing),
// and any error. On error the caller does not need to clean up.
func AddAudioStream(room *lksdk.Room, cfg utils.AudioPipelineConfig, fakeStream bool) (*utils.GStreamerPipeline, string, error) {
	if fakeStream {
		utils.Log.Infow("Audio stream: using sine-wave test source.")
	} else {
		utils.Log.Infow("Audio stream: using ALSA device.", "device", cfg.Device)
	}

	pipeline, err := utils.NewAudioPipeline(cfg, fakeStream)
	if err != nil {
		return nil, "", fmt.Errorf("create audio pipeline: %w", err)
	}

	track, pubSID, err := utils.PublishAudioTrack(room, cfg)
	if err != nil {
		pipeline.Free()
		return nil, "", fmt.Errorf("publish audio track: %w", err)
	}

	pipeline.AttachTrack(track)

	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		if unpubErr := room.LocalParticipant.UnpublishTrack(pubSID); unpubErr != nil {
			utils.Log.Warnw("Failed to unpublish track after pipeline start failure.", "error", unpubErr)
		}
		return nil, "", fmt.Errorf("start audio pipeline: %w", err)
	}

	return pipeline, pubSID, nil
}
