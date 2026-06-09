package main

import (
	"flag"
	"os"
	"strconv"
	"time"

	"racecast-emitter/scripts"
	"racecast-emitter/utils"

	"github.com/joho/godotenv"
)

var (
	debug        *bool
	fake         *bool
	noUPS        *bool
	noModem      *bool
	noMetadata   *bool
	fakeMetadata *bool
)

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
		addr64, err := strconv.ParseUint(os.Getenv("UPS_I2C_ADDR"), 0, 8)
		if err != nil {
			utils.Log.Fatalw("Invalid UPS_I2C_ADDR value.", "error", err)
		}
		bus, err := strconv.Atoi(os.Getenv("UPS_I2C_BUS"))
		if err != nil {
			utils.Log.Fatalw("Invalid UPS_I2C_BUS value.", "error", err)
		}
		scripts.CreateUPSReader(uint8(addr64), bus)
		defer scripts.CloseUPSReader()
	}

	if !*noModem && !*fake {
		scripts.SetupModem()
	}

	for {
		updateMetadata()
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
	noCam := flag.Bool("no-cam", false, "Disable all video streams")
	noMic := flag.Bool("no-mic", false, "Disable all audio streams")
	flag.Parse()

	utils.CreateLogger(debug)
	defer utils.Log.Sync()

	// Loading environment variables from .env file
	if godotenv.Load() != nil {
		utils.Log.Fatal(".env file not found.")
	}

	utils.SetLevelFromEnv(os.Getenv("LOG_LEVEL"))
	utils.Log.Infow("Launching program.", "process_id", os.Getpid())

	// Si l'adresse ou le bus de l'UPS n'existe pas dans le .env, on désactive la lecture de l'UPS
	if os.Getenv("UPS_I2C_ADDR") == "" || os.Getenv("UPS_I2C_BUS") == "" {
		utils.Log.Warn("UPS I2C address or bus not set in .env. Disabling UPS reader.")
		noUPS = utils.BoolPtr(true)
	}

	if !*noStream || (!*noMetadata && (!*noUPS || !*noModem)) {
		utils.SetupLiveKitRoom()
	}

	if !*noMetadata && (!*noUPS || !*noModem) {
		go roomMetadataUpdater()
	}

	if !*noStream && (!*noCam || !*noMic) {
		utils.InitGStreamer()

		rm := scripts.NewRecordManager()

		smOpts := scripts.StreamManagerOptions{
			NoCam:     *noCam,
			NoMic:     *noMic,
			Fake:      *fake || *fakeStream,
			RecordDir: rm.Dir(),
		}

		sm := scripts.NewStreamManager(smOpts)
		sm.Start()
		defer sm.Stop()
	}

	select {}
}