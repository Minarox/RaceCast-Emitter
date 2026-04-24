package utils

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"os"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	roomClient *lksdk.RoomServiceClient
	oldMetadata map[string]any
)

func getUrl(protocol string) string {
	domain := os.Getenv("LIVEKIT_DOMAIN")
	tls := os.Getenv("LIVEKIT_TLS") == "true"

	if domain == "" {
		Log.Fatalw("LIVEKIT_DOMAIN environment variable is not set. Please check your .env file.")
	}

	if !tls {
		return protocol + "://" + domain
	}
	return protocol + "s://" + domain
}

func createRoomClient() *lksdk.RoomServiceClient {
	if roomClient == nil {
		roomClient = lksdk.NewRoomServiceClient(
			getUrl("http"),
			os.Getenv("LIVEKIT_API_KEY"),
			os.Getenv("LIVEKIT_API_SECRET"),
		)
	}
	return roomClient
}

func CreateLiveKitClientToken() string {
	if os.Getenv("LIVEKIT_API_KEY") == "" || os.Getenv("LIVEKIT_API_SECRET") == "" || os.Getenv("LIVEKIT_ROOM") == "" || os.Getenv("LIVEKIT_IDENTITY") == "" {
		Log.Fatalw("LiveKit environment variables are not set. Please check your .env file.")
	}

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
		Log.Fatalw("Failed to generate LiveKit token.", "details", err)
	}

	return token
}

func SetupLiveKitRoom() {
	if os.Getenv("LIVEKIT_ROOM") == "" {
		Log.Fatalw("LIVEKIT_ROOM environment variable is not set. Please check your .env file.")
	}

	createRoomClient()

	var roomCreated bool = false
	rooms , err := roomClient.ListRooms(
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
		_, err = roomClient.CreateRoom(
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
		roomClient.UpdateRoomMetadata(
			context.Background(),
			&livekit.UpdateRoomMetadataRequest{
				Room:     os.Getenv("LIVEKIT_ROOM"),
				Metadata: string(payloadJSON),
			},
		)
	}
}