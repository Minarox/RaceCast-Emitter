package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"log"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
var livekitClientVersion = "2.15.2"

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

func systemStateReader() (*float32, *float32, *float32, *float32) {
	// System load
	var load *float32 = nil
	loadData, err := exec.Command("bash", "-c", `awk -v RS="" '{print 100-($5*100)/($2+$3+$4+$5+$6+$7+$8)}' <(head -n1 /proc/stat)`).Output()
	if err != nil {
		sugar.Errorw("Failed to read system load.", "details", err)
		return nil, nil, nil, nil
	}
	loadStr := strings.TrimSpace(string(loadData))
	loadValue, err := strconv.ParseFloat(loadStr, 32)
	if err != nil {
		sugar.Errorw("Failed to parse system load.", "details", err)
		return nil, nil, nil, nil
	}

	loadFloat := float32(loadValue)
	loadFloat = float32(math.Round(float64(loadFloat*100)) / 100)
	load = &loadFloat

	// CPU temperature
	var temperature *float32 = nil
	temperatureData, err := exec.Command("sh", "-c", `vcgencmd measure_temp | cut -d "=" -f2 | cut -d "'" -f1`).Output()
	if err != nil {
		sugar.Errorw("Failed to read system temperature.", "details", err)
		return nil, nil, nil, nil
	}

	temperatureStr := strings.TrimSpace(string(temperatureData))
	temperatureValue, err := strconv.ParseFloat(temperatureStr, 32)
	if err == nil {
		value := float32(temperatureValue)
		temperature = &value
	}


	// Fan speed
	sysDevicesPath := "/sys/devices/platform/cooling_fan"
	var fanInputFile string
	var fan *float32 = nil

	filepath.WalkDir(sysDevicesPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == "fan1_input" {
			fanInputFile = path
			return filepath.SkipDir
		}
		return nil
	})

	if fanInputFile != "" {
		data, err := os.ReadFile(fanInputFile)
		if err == nil {
			dataStr := strings.TrimSpace(string(data))

			value, err := strconv.ParseFloat(dataStr, 32)
			if err == nil {
				rpm := float32(value)
				fan = &rpm
			}
		}
	}

	// Total power consumption
	states, err := exec.Command("sh", "-c", `vcgencmd pmic_read_adc`).Output()
	if err != nil {
		sugar.Errorw("Failed to read system power consumption.", "details", err)
		return nil, nil, nil, nil
	}

	var currents = make(map[string]float32)
	var voltages = make(map[string]float32)

	lines := strings.SplitSeq(string(states), "\n")
	for line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Split(line, "=")
		label := strings.Fields(parts[0])[0]
		label = label[:len(label)-2]

		if strings.HasSuffix(parts[1], "A") {
			parts[1] = parts[1][:len(parts[1])-1]
			current, err := strconv.ParseFloat(parts[1], 32)
			if err != nil {
				sugar.Warnw("Failed to parse current value.", "details", err)
				continue
			}
			currents[label] = float32(current)
		}

		if strings.HasSuffix(parts[1], "V") {
			parts[1] = parts[1][:len(parts[1])-1]
			voltage, err := strconv.ParseFloat(parts[1], 32)
			if err != nil {
				sugar.Warnw("Failed to parse voltage value.", "details", err)
				continue
			}
			voltages[label] = float32(voltage)
		}
	}

	var watts float32
	for label, current := range currents {
		if voltage, ok := voltages[label]; ok {
			watts += current * voltage
		}
	}
	watts = float32(int(watts*100)) / 100

	return &watts, temperature, fan, load
}

func upsStateReader() (*float32, *float32) {
	i2cClient, err := i2c.NewI2C(0x36, 1)
	if err != nil {
		sugar.Fatalw("Failed to create I2C client for UPS.", "details", err)
	}
	defer i2cClient.Close()

	voltageBytes, _, err := i2cClient.ReadRegBytes(2, 2)
	if err != nil {
		sugar.Errorw("Failed to read voltage from UPS.", "details", err)
		return nil, nil
	}
	voltageRaw := binary.LittleEndian.Uint16(voltageBytes)
	voltageSwapped := (voltageRaw>>8)&0xFF | (voltageRaw&0xFF)<<8
	voltage := float32(voltageSwapped) * 1.25 / 1000 / 16
	voltage = float32(math.Round(float64(voltage*100)) / 100)

	capacityBytes, _, err := i2cClient.ReadRegBytes(4, 2)
	if err != nil {
		sugar.Errorw("Failed to read capacity from UPS.", "details", err)
		return &voltage, nil
	}
	capacityRaw := binary.LittleEndian.Uint16(capacityBytes)
	capacitySwapped := (capacityRaw>>8)&0xFF | (capacityRaw&0xFF)<<8
	capacity := float32(capacitySwapped) / 256
	capacity = float32(math.Round(float64(capacity*100)) / 100)

	return &voltage, &capacity
}

func mpuTemperatureUpdater() {
	i2cClient, err := i2c.NewI2C(0x68, 1)
	if err != nil {
		sugar.Fatalw("Failed to create I2C client for MPU.", "details", err)
	}
	defer i2cClient.Close()

	if err := i2cClient.WriteRegU8(0x6B, 0x00); err != nil {
		sugar.Fatalw("Failed to wake up MPU6050.", "details", err)
	}

	for {
		buf, _, err := i2cClient.ReadRegBytes(0x41, 2)
		if err != nil {
			sugar.Errorw("Failed to read temperature from MPU6050.", "details", err)
			time.Sleep(time.Millisecond * 1000)
			continue
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

func parsePrecision(nmea []string) (*float32, *float32) {
	for _, line := range nmea {
		if strings.HasPrefix(line, "$GPGGA") {
			parts := strings.Split(line, ",")
			if len(parts) > 8 {
				return parseFloat32(parts[7]), parseFloat32(parts[8])
			}
		}
	}
	return nil, nil
}

func roomMetadataUpdater() {
	logger.ChangePackageLogLevel("i2c", logger.LogLevel(sugar.Level()))
	go mpuTemperatureUpdater()

	modemID, err := exec.Command("sh", "-c", `mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`).Output()
	if err != nil {
		sugar.Fatalw("Failed to get modem ID.", "details", err)
	}

	_, err = exec.Command("sh", "-c", `mmcli -m `+string(modemID)+` --location-enable-gps-raw --location-enable-gps-nmea`).Output()
	if err != nil {
		sugar.Errorw("Failed to enable GPS.", "details", err)
	}

	// Connect to LiveKit server
	roomClient := lksdk.NewRoomServiceClient(
		"https://"+os.Getenv("LIVEKIT_DOMAIN"),
		os.Getenv("LIVEKIT_API_KEY"),
		os.Getenv("LIVEKIT_API_SECRET"),
	)

	for {
		// Parse modem data
		modemOutput, _ := exec.Command("sh", "-c", `mmcli -m `+string(modemID)+` -J`).Output()

		var modem Modem
		if err := json.Unmarshal(modemOutput, &modem); err != nil {
			sugar.Warnw("Error parsing modem data.", "details", err)
			return
		}

		tech := modem.Modem.Generic.AccessTechnologies
		signal := parseSignalQuality(modem.Modem.Generic.SignalQuality.Value)

		// Parse location data
		locationOutput, _ := exec.Command("sh", "-c", `mmcli -m `+string(modemID)+` --location-get -J`).Output()

		var location Modem
		if err := json.Unmarshal(locationOutput, &location); err != nil {
			sugar.Warnw("Error parsing location data.", "details", err)
			return
		}

		gps := location.Modem.Location.GPS
		longitude := parseFloat32(gps.Longitude)
		latitude := parseFloat32(gps.Latitude)
		altitude := parseFloat32(gps.Altitude)
		speed := parseSpeed(gps.NMEA)
		satellites, hdop := parsePrecision(gps.NMEA)

		// Read system state
		watts, temperature, fan, load := systemStateReader()

		// Read UPS state
		voltage, capacity := upsStateReader()

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
			"modem": map[string]any{
				"tech":   tech,
				"signal": signal,
			},
			"location": map[string]any{
				"long":  longitude,
				"lat":   latitude,
				"alt":   altitude,
				"speed": speed,
				"sat":   satellites,
				"hdop":  hdop,
			},
			"system": map[string]any{
				"watts":       watts,
				"temperature": temperature,
				"fan":         fan,
				"load":        load,
			},
			"ups": map[string]any{
				"voltage":  voltage,
				"capacity": capacity,
			},
			"temp": averageTemperature,
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
	if _, err := os.Stat("livekit-client@" + livekitClientVersion + ".umd.min.js"); os.IsNotExist(err) {
		sugar.Info("Downloading LiveKit client library (" + livekitClientVersion + ")...")

		cmd := exec.Command("curl", "-o", "livekit-client@"+livekitClientVersion+".umd.min.js", "https://cdn.jsdelivr.net/npm/livekit-client@"+livekitClientVersion+"/dist/livekit-client.umd.min.js")
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
	livekitScriptBytes, err := os.ReadFile("livekit-client@" + livekitClientVersion + ".umd.min.js")
	if err != nil {
		sugar.Fatalw("Failed to read LiveKit client library.", "details", err)
	}
	proto.PageAddScriptToEvaluateOnNewDocument{
		Source: string(livekitScriptBytes),
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
	config.OutputPaths = []string{
		"stdout",
		"logs/" + time.Now().Format(time.DateOnly) + ".log",
	}
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
