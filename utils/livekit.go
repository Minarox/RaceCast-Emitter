package utils

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"os"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	roomService *lksdk.RoomServiceClient
	roomCreated bool
	oldMetadata map[string]any
)

func getUrl() string {
	domain := os.Getenv("LIVEKIT_DOMAIN")
	tls := os.Getenv("LIVEKIT_TLS") == "true"

	if domain == "" {
		Log.Fatalw("LIVEKIT_DOMAIN environment variable is not set. Please check your .env file.")
	}

	if !tls {
		return "http://" + domain
	}
	return "https://" + domain
}

func createRoomService() *lksdk.RoomServiceClient {
	if roomService == nil {
		roomService = lksdk.NewRoomServiceClient(
			getUrl(),
			os.Getenv("LIVEKIT_API_KEY"),
			os.Getenv("LIVEKIT_API_SECRET"),
		)
	}
	return roomService
}

func SetupLiveKitRoom() {
	if (roomCreated) {
		return
	}

	if os.Getenv("LIVEKIT_ROOM") == "" {
		Log.Fatalw("LIVEKIT_ROOM environment variable is not set. Please check your .env file.")
	}

	createRoomService()

	rooms , err := roomService.ListRooms(
		context.Background(),
		&livekit.ListRoomsRequest{},
	)
	if err != nil {
		Log.Fatalw("Failed to list LiveKit rooms.", "details", err)
	}

	for _, room := range rooms.Rooms {
		if room.Name == os.Getenv("LIVEKIT_ROOM") {
			Log.Infow("LiveKit room already exists.", "room", os.Getenv("LIVEKIT_ROOM"))
			roomCreated = true
			return
		}
	}

	if !roomCreated {
		Log.Info("Creating LiveKit room...")
		_, err = roomService.CreateRoom(
			context.Background(),
			&livekit.CreateRoomRequest{
				Name: os.Getenv("LIVEKIT_ROOM"),
				DepartureTimeout: 60 * 60 * 24,
			},
		)
		if err != nil {
			Log.Fatalw("Failed to create LiveKit room.", "details", err)
		}

		Log.Infow("LiveKit room created.", "room", os.Getenv("LIVEKIT_ROOM"))
		roomCreated = true
	}
}

func UpdateLiveKitRoomMetadata(metadata map[string]any) {
	// Check if metadata has changed
	metadataJSON, _ := json.Marshal(metadata)
	oldMetadataJSON, _ := json.Marshal(oldMetadata)
	if oldMetadata == nil || sha256.Sum256(metadataJSON) != sha256.Sum256(oldMetadataJSON) {
		oldMetadata = metadata

		// Add current timestamp to metadata
		payload := maps.Clone(metadata)
		payload["timestamp"] = time.Now().Unix()
		payloadJSON, _ := json.Marshal(payload)

		Log.Debugw("Updating room metadata.", "payload", string(payloadJSON))

		// Update room metadata
		roomService.UpdateRoomMetadata(
			context.Background(),
			&livekit.UpdateRoomMetadataRequest{
				Room:     os.Getenv("LIVEKIT_ROOM"),
				Metadata: string(payloadJSON),
			},
		)
	}
}

func ConnectToLiveKitRoom() *lksdk.Room {
	SetupLiveKitRoom()

	room, err := lksdk.ConnectToRoom(getUrl(), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            os.Getenv("LIVEKIT_ROOM"),
		ParticipantIdentity: os.Getenv("LIVEKIT_IDENTITY"),
		ParticipantName:     os.Getenv("LIVEKIT_IDENTITY"),
	}, &lksdk.RoomCallback{})

	if err != nil {
		Log.Errorf("Failed to connect to LiveKit room: %v", err)
	}

	Log.Infow("Connected to LiveKit room.", "room", os.Getenv("LIVEKIT_ROOM"))
	return room
}