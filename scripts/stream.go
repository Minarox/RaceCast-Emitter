package scripts

import (
	"racecast-emitter/utils"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

// StartWebcamStream creates a GStreamer VP9 pipeline and publishes it to the
// given LiveKit room.
//
// The returned pipeline must be stopped and freed by the caller on shutdown.
func AddVideoStream(room *lksdk.Room, cfg utils.PipelineConfig, fakeStream bool) *utils.GStreamerPipeline {
	if fakeStream {
		utils.Log.Infow("Using SMPTE test pattern.")
	} else {
		utils.Log.Infow("Using webcam.", "device", cfg.Device)
	}

	pipeline, err := utils.NewVideoPipeline(cfg, fakeStream)
	if err != nil {
		utils.Log.Fatalw("Failed to create video pipeline.", "error", err)
	}

	track, err := utils.PublishVideoTrack(room, cfg.Name)
	if err != nil {
		pipeline.Free()
		utils.Log.Fatalw("Failed to publish video track.", "error", err)
	}

	pipeline.AttachTrack(track)

	// Force an IDR frame as soon as each new subscriber binds to the track,
	// so viewers never wait more than one keyframe interval for the first image.
	track.OnBind(func() {
		pipeline.ForceKeyframe()
	})

	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		utils.Log.Fatalw("Failed to start video pipeline.", "error", err)
	}

	utils.Log.Infow("Video stream started.")
	return pipeline
}

func AddAudioStream(room *lksdk.Room, cfg utils.AudioPipelineConfig, fakeStream bool) *utils.GStreamerPipeline {
	if fakeStream {
		utils.Log.Infow("Using fake audio stream (sine wave).")
	} else {
		utils.Log.Infow("Using microphone audio stream.")
	}

	pipeline, err := utils.NewAudioPipeline(cfg, fakeStream)
	if err != nil {
		utils.Log.Fatalw("Failed to create audio pipeline.", "error", err)
	}

	track, err := utils.PublishAudioTrack(room, cfg)
	if err != nil {
		pipeline.Free()
		utils.Log.Fatalw("Failed to publish audio track.", "error", err)
	}

	pipeline.AttachTrack(track)

	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		utils.Log.Fatalw("Failed to start audio pipeline.", "error", err)
	}

	utils.Log.Infow("Audio stream started.")
	return pipeline
}
