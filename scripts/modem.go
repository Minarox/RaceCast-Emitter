package scripts

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"racecast-emitter/utils"
	"strings"

	"golang.org/x/sys/unix"
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
	} `json:"modem"`
}

// atSerialPort is the AT command port for the Quectel RM520N-GL.
// USB layout: ttyUSB0=DM, ttyUSB1=NMEA, ttyUSB2=AT, ttyUSB3=AT(PPP), ttyUSB4=modem.
const atSerialPort = "/dev/ttyUSB3"

var modemID string

// sendATCommand sends an AT command directly to the modem's serial port,
// bypassing ModemManager which blocks mmcli --command outside of debug mode.
func sendATCommand(cmd string) (string, error) {
	f, err := os.OpenFile(atSerialPort, os.O_RDWR|unix.O_NOCTTY, 0600)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", atSerialPort, err)
	}
	defer f.Close()

	// Configure 115200 baud, 8N1, raw mode
	t := unix.Termios{}
	t.Iflag = unix.IGNPAR
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B115200
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 20 // 2-second read timeout (units of 0.1 s)
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, &t); err != nil {
		return "", fmt.Errorf("configure serial: %w", err)
	}

	if _, err := f.Write([]byte(cmd + "\r")); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return string(buf[:n]), nil
}

// parseGPSLocation retrieves GPS position via AT+QGPSLOC=2 sent directly to the serial port.
// AT+QGPSLOC=2 response fields (comma-separated after the prefix):
// UTC, lat, lon, hdop, alt, fix, cog, spkm, spkn, date, nsat
func parseGPSLocation() (lat, lon, alt, hdop, speed *float32, satellites *int) {
	out, err := sendATCommand("AT+QGPSLOC=2")
	if err != nil {
		utils.Log.Warnw("Cannot get GPS location.", "err", err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "+QGPSLOC:") {
			continue
		}
		idx := strings.Index(line, "+QGPSLOC:")
		data := strings.TrimSpace(line[idx+len("+QGPSLOC:"):])
		data = strings.Trim(data, "'\"")
		parts := strings.Split(data, ",")
		if len(parts) < 11 {
			break
		}
		lat = utils.ParseFloat32(parts[1])
		lon = utils.ParseFloat32(parts[2])
		hdop = utils.ParseFloat32(parts[3])
		alt = utils.ParseFloat32(parts[4])
		speed = utils.ParseFloat32(parts[7]) // spkm (km/h)
		satellites = utils.ParseInt(parts[10])
		break
	}
	return
}

func SetupModem() {
	modem, err := exec.Command("sh", "-c", `mmcli -L | grep 'Quectel' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`).Output()
	if err != nil {
		utils.Log.Fatalw("Failed to get modem ID.", "details", err)
	}
	modemID = string(modem)

	// Activate GPS directly via the AT serial port.
	// mmcli --command is blocked outside of debug mode; direct tty access bypasses this.
	// +CME ERROR: 504 means GPS is already active.
	response, err := sendATCommand("AT+QGPS=1")
	if err != nil {
		utils.Log.Errorw("Failed to send AT+QGPS=1.", "err", err)
	} else if !strings.Contains(response, "OK") && !strings.Contains(response, "504") {
		utils.Log.Errorw("Unexpected GPS enable response.", "response", response)
	}
}

func GetModemData() map[string]any {
	// Parse modem data
	modemOutput, _ := exec.Command("sh", "-c", `mmcli -m `+modemID+` -J`).Output()

	var modem Modem
	if err := json.Unmarshal(modemOutput, &modem); err != nil {
		utils.Log.Warnw("Error parsing modem data.", "details", err)
		return nil
	}

	tech := modem.Modem.Generic.AccessTechnologies
	signal := utils.ParseInt(modem.Modem.Generic.SignalQuality.Value)

	// Get GPS location via AT+QGPSLOC=2 (mmcli location API not supported on this modem)
	latitude, longitude, altitude, hdop, speed, satellites := parseGPSLocation()

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

	utils.Log.Infow("Modem Data", "payload", data)
	return data
}

func GetFakeModemData() map[string]any {
	// randomize around Paris by default
	baseLon := 2.349014
	baseLat := 48.864716

	// tech selection
	techs := []string{"LTE", "5G", "3G"}
	tech := techs[rand.Intn(len(techs))]

	signal := 30 + rand.Intn(71) // 30-100
	lon := baseLon + (rand.Float64()-0.5)/100.0 // small jitter
	lat := baseLat + (rand.Float64()-0.5)/100.0
	alt := 5.0 + rand.Float64()*50.0
	spd := rand.Float64() * 30.0
	sat := 4 + rand.Intn(9) // 4-12
	hdop := 0.5 + rand.Float64()*2.5

	return map[string]any{
		"tech":   tech,
		"signal": signal,
		"lon":    utils.ParseFloat32(fmt.Sprintf("%f", lon)),
		"lat":    utils.ParseFloat32(fmt.Sprintf("%f", lat)),
		"alt":    utils.ParseFloat32(fmt.Sprintf("%.0f", alt)),
		"spd":    utils.ParseFloat32(fmt.Sprintf("%.0f", spd)),
		"sat":    utils.ParseInt(fmt.Sprintf("%d", sat)),
		"hdop":   utils.ParseFloat32(fmt.Sprintf("%.2f", hdop)),
	}
}