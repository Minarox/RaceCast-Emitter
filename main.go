package main

import (
	"flag"
	"os"
	"time"

	"racecast-emitter/scripts"
	"racecast-emitter/utils"

	"github.com/joho/godotenv"
)

var (
	debug      *bool
	fake 	   *bool
	noUPS      *bool
	noGPS      *bool
	noMetadata *bool
)

var RoadCam = utils.PipelineConfig{
	Name:        "Route",
	Device:      "/dev/video0",
	Width:       1280,
	Height:      720,
	Framerate:   30,
	Bitrate:     1_000_000,
}

var InteriorCam = utils.PipelineConfig{
	Name:        "Habitacle",
	Device:      "/dev/video1",
	Width:       1280,
	Height:      720,
	Framerate:   30,
	Bitrate:     1_000_000,
}

// var PedalsCam = utils.PipelineConfig{
// 	Name:        "Pedals",
// 	Device:      "/dev/video2",
// 	Width:       1280,
// 	Height:      720,
// 	Framerate:   30,
// 	Bitrate:     1_000_000,
// }

func updateMetadata() {
	var (
		upsData map[string]any
		gpsData map[string]any
	)

	if !*noUPS {
		if !*fake {
			upsData = scripts.GetUPSData()
		} else {
			upsData = scripts.GetFakeUPSData()
		}
	}

	if !*noGPS {
		if !*fake {
			gpsData = scripts.GetGPSData()
		} else {
			gpsData = scripts.GetFakeGPSData()
		}
	}

	metadata := make(map[string]any)
	if upsData != nil {
		metadata["ups"] = upsData
	}
	if gpsData != nil {
		metadata["gps"] = gpsData
	}

	if len(metadata) > 0 {
		utils.UpdateLiveKitRoomMetadata(metadata)
	}
}

func roomMetadataUpdater() {
	if !*noUPS && !*fake {
		scripts.CreateUPSReader(0x41, 1)
		defer scripts.CloseUPSReader()
	}

	if !*noGPS && !*fake {
		scripts.SetupGPS()
	}

	for {
		go updateMetadata()
		time.Sleep(time.Second)
	}
}

func main() {
	// Read command line flags
	debug = flag.Bool("debug", false, "Enable debug mode")
	fake = flag.Bool("fake", false, "Enable fake mode")
	noUPS = flag.Bool("no-ups", false, "Disable UPS state reader")
	noGPS = flag.Bool("no-gps", false, "Disable GPS state reader")
	noMetadata = flag.Bool("no-metadata", false, "Disable metadata updates")
	noStream := flag.Bool("no-stream", false, "Disable video/audio streaming")
	fakeStream := flag.Bool("fake-stream", false, "Use fake video/audio stream")
	flag.Parse()

	utils.CreateLogger(debug)
	defer utils.Log.Sync()

	// Loading environment variables from .env file
	if godotenv.Load() != nil {
		utils.Log.Fatal(".env file not found.")
	}

	utils.SetLevelFromEnv(os.Getenv("LOG_LEVEL"))
	utils.Log.Infow("Launching program.", "process_id", os.Getpid())

	if (!*noStream || (!*noMetadata && (!*noUPS || !*noGPS))) {
		utils.SetupLiveKitRoom()
	}

	if !*noMetadata && (!*noUPS || !*noGPS) {
		go roomMetadataUpdater()
	}

	if !*noStream {
		utils.InitGStreamer()

		room := utils.ConnectToLiveKitRoom()
		defer room.Disconnect()

		RoadPipeline := scripts.AddVideoStream(room, RoadCam, *fake || *fakeStream)
		defer RoadPipeline.Stop()
		defer RoadPipeline.Free()

		InteriorPipeline := scripts.AddVideoStream(room, InteriorCam, *fake || *fakeStream)
		defer InteriorPipeline.Stop()
		defer InteriorPipeline.Free()

		// PedalsPipeline := scripts.AddVideoStream(room, PedalsCam, true)
		// defer PedalsPipeline.Stop()
		// defer PedalsPipeline.Free()
	}

	select {}
}