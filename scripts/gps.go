package scripts

import (
	"encoding/json"
	"os/exec"
	"racecast-emitter/utils"
	"strings"
)

// https://www.waveshare.com/wiki/RM520N-GL_5G_for_Jetson_Nano

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

var modemID string

func parseSpeed(nmea []string) *float32 {
	for _, line := range nmea {
		if strings.HasPrefix(line, "$GPVTG") {
			parts := strings.Split(line, ",")
			if len(parts) > 7 {
				return utils.ParseFloat32(parts[7])
			}
		}
	}
	return nil
}

func parsePrecision(nmea []string) (*int, *float32) {
	for _, line := range nmea {
		if strings.HasPrefix(line, "$GPGGA") {
			parts := strings.Split(line, ",")
			if len(parts) > 8 {
				return utils.ParseInt(parts[7]), utils.ParseFloat32(parts[8])
			}
		}
	}
	return nil, nil
}

func SetupGPS() {
	modem, err := exec.Command("sh", "-c", `mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`).Output()
	if err != nil {
		utils.Log.Fatalw("Failed to get modem ID.", "details", err)
	}
	modemID = string(modem)

	_, err = exec.Command("sh", "-c", `mmcli -m `+modemID+` --location-enable-gps-raw --location-enable-gps-nmea`).Output()
	if err != nil {
		utils.Log.Errorw("Failed to enable GPS.", "details", err)
	}
}

func GetGPSData() map[string]any {
	// Parse modem data
	modemOutput, _ := exec.Command("sh", "-c", `mmcli -m `+modemID+` -J`).Output()

	var modem Modem
	if err := json.Unmarshal(modemOutput, &modem); err != nil {
		utils.Log.Warnw("Error parsing modem data.", "details", err)
		return nil
	}

	tech := modem.Modem.Generic.AccessTechnologies
	signal := utils.ParseInt(modem.Modem.Generic.SignalQuality.Value)

	// Parse location data
	locationOutput, _ := exec.Command("sh", "-c", `mmcli -m `+modemID+` --location-get -J`).Output()

	var location Modem
	if err := json.Unmarshal(locationOutput, &location); err != nil {
		utils.Log.Warnw("Error parsing location data.", "details", err)
		return nil
	}

	gps := location.Modem.Location.GPS
	longitude := utils.ParseFloat32(gps.Longitude)
	latitude := utils.ParseFloat32(gps.Latitude)
	altitude := utils.ParseFloat32(gps.Altitude)
	speed := parseSpeed(gps.NMEA)
	satellites, hdop := parsePrecision(gps.NMEA)

	var data = map[string]any{
		"tech":   tech,
		"signal": signal,
		"lon":    longitude,
		"lat":    latitude,
		"alt":    altitude,
		"spd":    speed,
		"sat":    satellites,
		"hdop":   hdop,
	}

	utils.Log.Infow("GPS Data", "payload", data)
	return data
}