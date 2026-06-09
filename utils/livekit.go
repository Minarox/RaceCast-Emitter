package utils

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

// newPionLogger builds a LiveKit/Pion logger wired to our existing Zap logger
// and capped at the level defined by the LOG_LEVEL environment variable so that
// Pion's ICE/DTLS/RTP traces respect the same verbosity as the application.
func newPionLogger() protoLogger.Logger {
	// Map our LOG_LEVEL values to the subset understood by protoLogger.Config.
	// "verbose" has no Pion equivalent, so it maps to "info".
	levelMap := map[string]string{
		"debug":   "debug",
		"verbose": "info",
		"info":    "info",
		"warn":    "warn",
		"error":   "error",
	}
	level, ok := levelMap[os.Getenv("LOG_LEVEL")]
	if !ok {
		level = "warn"
	}

	l, err := protoLogger.FromZapLogger(
		Log.Desugar(),
		&protoLogger.Config{Level: level},
	)
	if err != nil {
		// Should never happen with a valid config; fall through to default.
		Log.Warnw("Failed to create Pion logger; Pion logs will be unfiltered.", "error", err)
		return protoLogger.GetLogger()
	}
	return l
}

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
	if roomCreated {
		return
	}

	if os.Getenv("LIVEKIT_ROOM") == "" {
		Log.Fatalw("LIVEKIT_ROOM environment variable is not set. Please check your .env file.")
	}

	createRoomService()

	rooms, err := roomService.ListRooms(
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
		_, err := roomService.UpdateRoomMetadata(
			context.Background(),
			&livekit.UpdateRoomMetadataRequest{
				Room:     os.Getenv("LIVEKIT_ROOM"),
				Metadata: string(payloadJSON),
			},
		)
		if err != nil {
			Log.Warnw("Failed to update room metadata.", "error", err)
		}
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
	}, &lksdk.RoomCallback{}, lksdk.WithLogger(newPionLogger()))

	if err != nil {
		Log.Errorw("Failed to connect to LiveKit room.", "error", err)
	}

	Log.Infow("Connected to LiveKit room.", "room", os.Getenv("LIVEKIT_ROOM"))
	return room
}

// TryConnectToLiveKitRoom attempts to connect to the LiveKit room using the
// provided callback.  Unlike ConnectToLiveKitRoom it returns an error instead
// of logging a fatal, so callers can retry on failure.
func TryConnectToLiveKitRoom(cb *lksdk.RoomCallback) (*lksdk.Room, error) {
	SetupLiveKitRoom()
	if cb == nil {
		cb = &lksdk.RoomCallback{}
	}
	room, err := lksdk.ConnectToRoom(getUrl(), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            os.Getenv("LIVEKIT_ROOM"),
		ParticipantIdentity: os.Getenv("LIVEKIT_IDENTITY"),
		ParticipantName:     os.Getenv("LIVEKIT_IDENTITY"),
	}, cb, lksdk.WithLogger(newPionLogger()))
	if err != nil {
		return nil, err
	}
	Log.Infow("Connected to LiveKit room.", "room", os.Getenv("LIVEKIT_ROOM"))
	return room, nil
}

// PublishVideoTrack creates and publishes a video LocalSampleTrack to the room.
// Returns the track, publication SID (needed to unpublish later), and any error.
func PublishVideoTrack(room *lksdk.Room, trackName string) (*lksdk.LocalSampleTrack, string, error) {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeAV1,
		ClockRate: 90000,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to create video sample track: %w", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   trackName,
		Source: livekit.TrackSource_CAMERA,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to publish video track: %w", err)
	}
	return track, pub.SID(), nil
}

// PublishAudioTrack creates and publishes an audio LocalSampleTrack to the room.
// Returns the track, publication SID (needed to unpublish later), and any error.
func PublishAudioTrack(room *lksdk.Room, trackName string) (*lksdk.LocalSampleTrack, string, error) {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to create audio sample track: %w", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   trackName,
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to publish audio track: %w", err)
	}
	return track, pub.SID(), nil
}