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
	debug        *bool
	fake 	     *bool
	noUPS        *bool
	noModem      *bool
	noMetadata   *bool
	fakeMetadata *bool
)

var RoadCam = utils.PipelineConfig{
	Name:        "Route",
	Device:      "/dev/video0",
	Width:       640,
	Height:      480,
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

var PedalsCam = utils.PipelineConfig{
	Name:        "Pedals",
	Device:      "/dev/video2",
	Width:       1280,
	Height:      720,
	Framerate:   30,
	Bitrate:     1_000_000,
}

func updateMetadata() {
	var (
		upsData map[string]any
		modemData map[string]any
	)

	if !*noUPS {
		if *fake || *fakeMetadata {
			upsData = scripts.GetFakeUPSData()
		} else {
			upsData = scripts.GetUPSData()
		}
	}

	if !*noModem {
		if *fake || *fakeMetadata {
			modemData = scripts.GetFakeModemData()
		} else {
			modemData = scripts.GetModemData()
		}
	}

	metadata := make(map[string]any)
	if upsData != nil {
		metadata["ups"] = upsData
	}
	if modemData != nil {
		metadata["modem"] = modemData
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

	if !*noModem && !*fake {
		scripts.SetupModem()
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
	noModem = flag.Bool("no-modem", false, "Disable Modem state reader")
	noMetadata = flag.Bool("no-metadata", false, "Disable metadata updates")
	fakeMetadata = flag.Bool("fake-metadata", false, "Use fake metadata")
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

	if (!*noStream || (!*noMetadata && (!*noUPS || !*noModem))) {
		utils.SetupLiveKitRoom()
	}

	if !*noMetadata && (!*noUPS || !*noModem) {
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