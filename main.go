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
	"strings"
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
	"go.uber.org/zap"
)

var sugar *zap.SugaredLogger
var temperatureReadings [32]float32
var oldMetadata any

type Modem struct {
	Modem struct {
		Generic struct {
			AccessTechnologies any `json:"access-technologies"`
			SignalQuality      struct {
				Value string `json:"value"`
			} `json:"signal-quality"`
		} `json:"generic"`
		Location struct {
			GPS struct {
				Longitude string   `json:"longitude"`
				Latitude  string   `json:"latitude"`
				Altitude  string   `json:"altitude"`
				NMEA      []string `json:"nmea"`
			} `json:"gps"`
		} `json:"location"`
	} `json:"modem"`
}

func mpuTemperatureUpdater() {
	logger.ChangePackageLogLevel("i2c", logger.LogLevel(sugar.Level()))
	i2cClient, err := i2c.NewI2C(0x68, 1)
	if err != nil {
		sugar.Fatalw("Failed to create I2C client.", "details", err)
	}
	defer i2cClient.Close()

	if err := i2cClient.WriteRegU8(0x6B, 0x00); err != nil {
		sugar.Fatalw("Failed to wake up MPU6050.", "details", err)
	}

	for {
		buf, _, err := i2cClient.ReadRegBytes(0x41, 2)
		if err != nil {
			sugar.Errorw("Failed to read temperature from MPU6050.", "details", err)
		}

		rawTemp := int16(binary.BigEndian.Uint16(buf))
		temperature := float32(rawTemp)/340.0 + 36.53
		copy(temperatureReadings[0:], temperatureReadings[1:])
		temperatureReadings[len(temperatureReadings)-1] = temperature

		time.Sleep(time.Millisecond * 100)
	}
}

func parseFloat32(s string) *float32 {
	if s == "" {
		return nil
	}
	if val, err := strconv.ParseFloat(s, 32); err == nil {
		v := float32(val)
		return &v
	}
	return nil
}

func parseSignalQuality(s string) *int {
	if s == "" {
		return nil
	}
	if val, err := strconv.Atoi(s); err == nil {
		return &val
	}
	return nil
}

func parseSpeed(nmea []string) *float32 {
	for _, line := range nmea {
		if strings.HasPrefix(line, "$GPVTG") {
			parts := strings.Split(line, ",")
			if len(parts) > 7 {
				return parseFloat32(parts[7])
			}
		}
	}
	return nil
}

func roomMetadataUpdater() {
	go mpuTemperatureUpdater()

	modemID, err := exec.Command("sh", "-c", `mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`).Output()
	if err != nil {
		sugar.Fatalw("Failed to get modem ID.", "details", err)
	}

	_, err = exec.Command("sh", "-c", `mmcli -m ` + string(modemID) + ` --location-enable-gps-raw --location-enable-gps-nmea`).Output()
	if err != nil {
		sugar.Errorw("Failed to enable GPS.", "details", err)
	}

	// Connect to LiveKit server
	roomClient := lksdk.NewRoomServiceClient(
		"https://" + os.Getenv("LIVEKIT_DOMAIN"),
		os.Getenv("LIVEKIT_API_KEY"),
		os.Getenv("LIVEKIT_API_SECRET"),
	)

	for {
		// Parse modem data
		modemOutput, _ := exec.Command("sh", "-c", `mmcli -m ` + string(modemID) + ` -J`).Output()

		var modem Modem
		if err := json.Unmarshal(modemOutput, &modem); err != nil {
			sugar.Warnw("Error parsing modem data.", "details", err)
			return
		}

		tech := modem.Modem.Generic.AccessTechnologies
		signal := parseSignalQuality(modem.Modem.Generic.SignalQuality.Value)

		// Parse location data
		locationOutput, _ := exec.Command("sh", "-c", `mmcli -m ` + string(modemID) + ` --location-get -J`).Output()

		var location Modem
		if err := json.Unmarshal(locationOutput, &location); err != nil {
			sugar.Warnw("Error parsing location data.", "details", err)
			return
		}

		gps := location.Modem.Location.GPS
		long := parseFloat32(gps.Longitude)
		lat := parseFloat32(gps.Latitude)
		alt := parseFloat32(gps.Altitude)
		speed := parseSpeed(gps.NMEA)

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

			sugar.Debugw("Updating room metadata.", "payload", string(payloadJSON))

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
		sugar.Info("Downloading LiveKit client library...")

		cmd := exec.Command("curl", "-o", "livekit-client.umd.min.js", "https://cdn.jsdelivr.net/npm/livekit-client@2.13.6/dist/livekit-client.umd.min.js")
		if err := cmd.Run(); err != nil {
			sugar.Fatalw("Failed to download LiveKit client library.", "details", err)
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
		sugar.Fatalw("Failed to generate LiveKit token.", "details", err)
	}

	os.Setenv("LIVEKIT_CLIENT_TOKEN", token)
}

func publishStreams() {
	downloadLiveKitClient()
	setLiveKitClientToken()

	path, exists := launcher.LookPath()
	if !exists {
		sugar.Warnw("Chromium browser launcher not found. Downloading...")
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
		sugar.Fatalw("Failed to read LiveKit client library.", "details", err)
	}
	proto.PageAddScriptToEvaluateOnNewDocument{
		Source: string(scriptBytes),
	}.Call(page)

	page.MustExpose("env", func(v gson.JSON) (any, error) {
		return os.Getenv(v.Str()), nil
	})

	page.MustExpose("logDebug", func(v gson.JSON) (any, error) {
		sugar.Debugw("LiveKit client.", "message", v.Str())
		return nil, nil
	})

	page.MustExpose("logInfo", func(v gson.JSON) (any, error) {
		sugar.Infow("LiveKit client.", "message", v.Str())
		return nil, nil
	})

	page.MustExpose("logWarn", func(v gson.JSON) (any, error) {
		sugar.Warnw("LiveKit client.", "message", v.Str())
		return nil, nil
	})

	page.
		MustNavigate("https://" + os.Getenv("LIVEKIT_DOMAIN")).
		MustEval(`async () => {
			const url = "wss://" + await window.env('LIVEKIT_DOMAIN');
			const token = await window.env("LIVEKIT_CLIENT_TOKEN");
			const room = new LivekitClient.Room({
				reconnectPolicy: {
					nextRetryDelayInMs: () => {
						return 1000;
					}
				}
			});

			room.prepareConnection(url, token);

			const createAudioTrack = async (device) => {
				await room.localParticipant.publishTrack(
					await LivekitClient.createLocalAudioTrack({
						deviceId: device.deviceId,
						autoGainControl: false,
						echoCancellation: false,
						noiseSuppression: false
					}),
					{
						name: device.label,
						stream: device.groupId,
						simulcast: false,
						source: LivekitClient.Track.Source.Microphone,
						red: false,
						dtx: true,
						stopMicTrackOnMute: false,
						/* audioPreset: {
							maxBitrate: 36_000
						} */
					}
				);
			};

			const createVideoTrack = async (device) => {
				await room.localParticipant.publishTrack(
					await LivekitClient.createLocalVideoTrack({
						deviceId: device.deviceId
					}),
					{
						name: device.label,
						stream: device.groupId,
						simulcast: false,
						source: LivekitClient.Track.Source.Camera,
						degradationPreference: "maintain-framerate",
						videoCodec: "VP8",
						/* videoEncoding: {
							maxFramerate: 30,
							maxBitrate: 2_500_000,
						} */
					}
				);
			};

			// Create audio and video tracks
			const devices = (await navigator.mediaDevices.enumerateDevices())
				.filter(device =>['audioinput', 'videoinput'].includes(device.kind) && device.deviceId !== 'default');
			const audioDevices = devices.filter(device => device.kind === 'audioinput');
			const videoDevices = devices.filter(device => device.kind === 'videoinput');

			audioDevices.forEach(async (device) => await createAudioTrack(device));
			videoDevices.forEach(async (device) => await createVideoTrack(device));

			room
				.on(LivekitClient.RoomEvent.LocalTrackPublished, async (track) => {
					await window.logDebug(
						JSON.stringify({ name: track.trackName, type: track.kind })
					);
				})
				.on(LivekitClient.RoomEvent.Connected, async () => {
					await window.logInfo("Connected");
				})
				.on(LivekitClient.RoomEvent.Reconnecting, async () => {
					await window.logWarn("Reconnecting...");
				})
				.on(LivekitClient.RoomEvent.Reconnected, async () => {
					await window.logInfo("Reconnected");
				})
				.on(LivekitClient.RoomEvent.Disconnected, async () => {
					await window.logWarn("Disconnected");
				});

			await room.connect(url, token);
		}`)

	select {}
}

func main() {
	// Loading logger
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	config.OutputPaths = []string{"stdout", "app.log"}
    logger, err := config.Build()

    if err != nil {
        log.Fatalf("Failed to build logger: %v", err)
    }

    sugar = logger.Sugar()
	sugar.Debugw("Launching program.", "process_id", os.Getpid())

	// Loading environment variables from .env file
	if godotenv.Load() != nil {
		sugar.Fatal(".env file not found.")
	}

	// Update room metadata with modem, location, and temperature data
	go roomMetadataUpdater()

	// Publish audio and video streams to LiveKit
	publishStreams()
}
