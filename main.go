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
	noUPS      *bool
	noGPS      *bool
	noMetadata *bool
	noStream   *bool
)

func updateMetadata() {
	var (
		upsData map[string]any
		gpsData map[string]any
	)

	if !*noUPS {
		upsData = scripts.GetUPSData()
	}

	if !*noGPS {
		gpsData = scripts.GetGPSData()
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
	if !*noUPS {
		scripts.CreateUPSReader(0x41, 1)
		defer scripts.CloseUPSReader()
	}

	if !*noGPS {
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
	noUPS = flag.Bool("no-ups", false, "Disable UPS state reader")
	noGPS = flag.Bool("no-gps", false, "Disable GPS state reader")
	noMetadata = flag.Bool("no-metadata", false, "Disable metadata updates")
	noStream = flag.Bool("no-stream", false, "Disable video/audio streaming")
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
		room := utils.ConnectToLiveKitRoom()
		defer room.Disconnect()

		// TODO: Create stream publisher and start streaming video/audio tracks to LiveKit room.
	}

	select {}
}