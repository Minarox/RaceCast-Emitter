package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strconv"
	"time"

	"maps"

	i2c "github.com/d2r2/go-i2c"
	"github.com/d2r2/go-logger"
	"github.com/joho/godotenv"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var temperatureReadings [32]float32
var oldMetadata any

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Unable to start the application: .env file not found.")
	}

	go roomMetadataUpdater()
	// go publisher()

	select {}
}

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

	roomClient := lksdk.NewRoomServiceClient(
		os.Getenv("LIVEKIT_URL"),
		os.Getenv("LIVEKIT_API_KEY"),
		os.Getenv("LIVEKIT_API_SECRET"),
	)

	for {
		// Parse modem data
		modemBytes, _ := exec.Command("sh", "-c", `mmcli -m `+string(modemID)+` -J`).Output()

		var modemData map[string]any
		var tech any = nil
		var signal any = nil
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
		locationBytes, _ := exec.Command("sh", "-c", `mmcli -m `+string(modemID)+` --location-get -J`).Output()

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
		averageTemperature := float32(int((sum / float32(count)) * 10)) / 10

		metadata := map[string]any{
			"tech":   tech,
			"signal": signal,
			"long":   long,
			"lat":    lat,
			"alt":    alt,
			"speed":  speed,
			"temp":   averageTemperature,
		}

		metadataJSON, _ := json.Marshal(metadata)
		oldMetadataJSON, _ := json.Marshal(oldMetadata)
		if oldMetadata == nil || sha256.Sum256(metadataJSON) != sha256.Sum256(oldMetadataJSON) {
		    oldMetadata = metadata

			// Add current timestamp to metadata
			payload := maps.Clone(metadata)
			payload["timestamp"] = time.Now().Unix()
			payloadJSON, _ := json.Marshal(payload)

			roomClient.UpdateRoomMetadata(
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

func publisher() {
	// room, err := lksdk.ConnectToRoom(
	// 	os.Getenv("LIVEKIT_URL"),
	// 	lksdk.ConnectInfo{
	// 		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
	// 		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
	// 		RoomName:            os.Getenv("LIVEKIT_ROOM"),
	// 		ParticipantIdentity: os.Getenv("LIVEKIT_IDENTITY"),
	// 	},
	// 	&lksdk.RoomCallback{},
	// )
	// if err != nil {
	// 	log.Fatalf("Unable to connect to LiveKit room: %v", err)
	// }

	// log.Printf("Connection state: %s", room.ConnectionState())
	// log.Printf("Connected to room: %s", room.Name())

	// room.Disconnect()
}
