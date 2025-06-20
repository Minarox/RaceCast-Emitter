package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"log"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"time"

	i2c "github.com/d2r2/go-i2c"
	"github.com/d2r2/go-logger"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/joho/godotenv"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/ysmood/gson"
)

var temperatureReadings [32]float32
var oldMetadata any

func mpuTemperatureUpdater() {
	logger.ChangePackageLogLevel("i2c", logger.InfoLevel)
	i2cClient, err := i2c.NewI2C(0x68, 1)
	if err != nil {
		log.Fatalf("Unable to create I2C client: %v", err)
	}
	defer i2cClient.Close()

	if err := i2cClient.WriteRegU8(0x6B, 0x00); err != nil {
		log.Fatalf("Failed to wake up MPU6050: %v", err)
	}

	for {
		buf, _, err := i2cClient.ReadRegBytes(0x41, 2)
		if err != nil {
			log.Printf("Failed to read temperature: %v", err)
		}

		rawTemp := int16(binary.BigEndian.Uint16(buf))
		temperature := float32(rawTemp)/340.0 + 36.53
		copy(temperatureReadings[0:], temperatureReadings[1:])
		temperatureReadings[len(temperatureReadings)-1] = temperature

		time.Sleep(time.Millisecond * 100)
	}
}

func roomMetadataUpdater() {
	go mpuTemperatureUpdater()

	modemID, err := exec.Command("sh", "-c", `mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`).Output()
	if err != nil {
		log.Printf("Failed to get modem ID: %v", err)
	}

	// Connect to LiveKit server
	roomClient := lksdk.NewRoomServiceClient(
		"https://" + os.Getenv("LIVEKIT_DOMAIN"),
		os.Getenv("LIVEKIT_API_KEY"),
		os.Getenv("LIVEKIT_API_SECRET"),
	)

	for {
		// Parse modem data
		modemBytes, _ := exec.Command("sh", "-c", `mmcli -m ` + string(modemID) + ` -J`).Output()

		var modemData map[string]any
		var tech, signal any = nil, nil
		if err := json.Unmarshal(modemBytes, &modemData); err == nil {
			if modem, ok := modemData["modem"].(map[string]any); ok {
				if generic, ok := modem["generic"].(map[string]any); ok {
					if accessTechnologies, ok := generic["access-technologies"]; ok {
						tech = accessTechnologies
					}
					if signalQuality, ok := generic["signal-quality"].(map[string]any); ok {
						if value, ok := signalQuality["value"].(string); ok {
							if parsedValue, err := strconv.Atoi(value); err == nil {
								signal = parsedValue
							}
						}
					}
				}
			}
		}

		// Parse location data
		locationBytes, _ := exec.Command("sh", "-c", `mmcli -m ` + string(modemID) + ` --location-get -J`).Output()

		var locationData map[string]any
		var long, lat, alt, speed any = nil, nil, nil, nil
		if err := json.Unmarshal(locationBytes, &locationData); err == nil {
			if location, ok := locationData["location"].(map[string]any); ok {
				if longitude, ok := location["longitude"].(string); ok && len(longitude) > 0 {
					if value, err := strconv.ParseFloat(longitude, 32); err == nil {
						long = float32(value)
					}
				}
				if latitude, ok := location["latitude"].(string); ok && len(latitude) > 0 {
					if value, err := strconv.ParseFloat(latitude, 32); err == nil {
						lat = float32(value)
					}
				}
				if altitude, ok := location["altitude"].(string); ok && len(altitude) > 0 {
					if value, err := strconv.ParseFloat(altitude, 32); err == nil {
						alt = float32(value)
					}
				}
			}
			// if nmea, ok := location["nmea"].([]any); ok {
			// 	for _, nmeaItem := range nmea {
			// 		if nmeaStr, ok := nmeaItem.(string); ok && len(nmeaStr) > 0 && nmeaStr[:6] == "$GPVTG" {
			// 			parts := splitNMEA(nmeaStr)
			// 			if len(parts) > 7 {
			// 				if speedStr := parts[7]; speedStr != "" {
			// 					if parsedSpeed, err := strconv.ParseFloat(speedStr, 32); err == nil {
			// 						speed = float32(parsedSpeed)
			// 					}
			// 				}
			// 			}
			// 		}
			// 	}
			// }
		}

		// Compute average temperature
		var sum float32
		var count int
		for _, t := range temperatureReadings {
			if t != 0 {
				sum += t
				count++
			}
		}
		averageTemperature := float32(int((sum/float32(count))*10)) / 10

		// Create metadata payload
		metadata := map[string]any{
			"tech":   tech,
			"signal": signal,
			"long":   long,
			"lat":    lat,
			"alt":    alt,
			"speed":  speed,
			"temp":   averageTemperature,
		}

		// Check if metadata has changed
		metadataJSON, _ := json.Marshal(metadata)
		oldMetadataJSON, _ := json.Marshal(oldMetadata)
		if oldMetadata == nil || sha256.Sum256(metadataJSON) != sha256.Sum256(oldMetadataJSON) {
			oldMetadata = metadata

			// Add current timestamp to metadata
			payload := maps.Clone(metadata)
			payload["timestamp"] = time.Now().Unix()
			payloadJSON, _ := json.Marshal(payload)

			// log.Printf("Updating room metadata: %s", string(payloadJSON))

			// Update room metadata
			go roomClient.UpdateRoomMetadata(
				context.Background(),
				&livekit.UpdateRoomMetadataRequest{
					Room:     os.Getenv("LIVEKIT_ROOM"),
					Metadata: string(payloadJSON),
				},
			)
		}

		time.Sleep(time.Second)
	}
}

func downloadLiveKitClient() {
	// Download the LiveKit client library if not already present
	if _, err := os.Stat("livekit-client.umd.min.js"); os.IsNotExist(err) {
		log.Println("Downloading LiveKit client library...")

		cmd := exec.Command("curl", "-o", "livekit-client.umd.min.js", "https://cdn.jsdelivr.net/npm/livekit-client@2.13.6/dist/livekit-client.umd.min.js")
		if err := cmd.Run(); err != nil {
			log.Fatalf("Failed to download LiveKit client library: %v", err)
		}
	}
}

func setLiveKitClientToken() {
	at := auth.NewAccessToken(os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET"))
	grant := &auth.VideoGrant{
		RoomCreate: true,
		RoomJoin:   true,
		Room:       os.Getenv("LIVEKIT_ROOM"),
	}

	at.SetVideoGrant(grant).
		SetIdentity(os.Getenv("LIVEKIT_IDENTITY")).
		SetValidFor(time.Hour * 24)

	token, err := at.ToJWT()
	if err != nil {
		log.Fatalf("Failed to generate LiveKit token: %v", err)
	}

	os.Setenv("LIVEKIT_CLIENT_TOKEN", token)
}

func publishStreams() {
	downloadLiveKitClient()
	setLiveKitClientToken()

	path, exists := launcher.LookPath()
	if !exists {
		log.Println("Chromium browser launcher not found. Downloading...")
	}

	url := launcher.
		NewUserMode().
		Bin(path).
		Leakless(true).
		Devtools(false).
		Headless(false).
		HeadlessNew(true).
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-search-engine-choice-screen").
		Set("ash-no-nudges").
		Set("disable-features", "Translate,WebRtcPipeWireCamera").
		Set("use-fake-ui-for-media-stream").
		MustLaunch()

	browser := rod.New().ControlURL(url).MustConnect()
	defer browser.MustClose()

	// [DEBUG] Check hardware acceleration support
	// browser.MustPage("chrome://gpu").MustWaitLoad().MustPDF("sample.pdf")

	page, _ := browser.Page(proto.TargetCreateTarget{})

	// Inject livekit-client.umd.min.js into the page
	scriptBytes, err := os.ReadFile("livekit-client.umd.min.js")
	if err != nil {
		log.Fatalf("Failed to read livekit-client.umd.min.js: %v", err)
	}
	proto.PageAddScriptToEvaluateOnNewDocument{
		Source: string(scriptBytes),
	}.Call(page)

	page.MustExpose("env", func(v gson.JSON) (any, error) {
		return os.Getenv(v.Str()), nil
	})

	page.MustExpose("log", func(v gson.JSON) (any, error) {
		log.Println(v.Str())
		return nil, nil
	})

	page.
		MustNavigate("https://" + os.Getenv("LIVEKIT_DOMAIN")).
		MustEval(`async () => {
			const url = "wss://" + await window.env('LIVEKIT_DOMAIN');
			const token = await window.env("LIVEKIT_CLIENT_TOKEN");

			// Create audio and video tracks
			const devices = (await navigator.mediaDevices.enumerateDevices())
				.filter(device =>['audioinput', 'videoinput'].includes(device.kind) && device.deviceId !== 'default');
			const audioDevices = devices.filter(device => device.kind === 'audioinput');
			const videoDevices = devices.filter(device => device.kind === 'videoinput');

			await window.log(JSON.stringify(audioDevices, null, 2));
			await window.log(JSON.stringify(videoDevices, null, 2));

			await window.log(url);
			await window.log(token);
		}`)

	select {}
}

func main() {
	// Loading environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Unable to start the application: .env file not found.")
	}

	// Update room metadata with modem, location, and temperature data
	go roomMetadataUpdater()

	// Publish audio and video streams to LiveKit
	publishStreams()
}
