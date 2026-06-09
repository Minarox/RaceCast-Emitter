package utils

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AudioDeviceInfo represents a discovered ALSA capture device.
type AudioDeviceInfo struct {
	CardNum int
	DevNum  int
	Name    string
	Device  string // e.g. "hw:2,0"
}

var cardTypeRe = regexp.MustCompile(`(?i)Card type\s*:\s*(.+)`)

// GetVideoDeviceName returns the human-readable card name reported by v4l2-ctl,
// or the basename of the device path as a fallback.
func GetVideoDeviceName(device string) string {
	out, err := exec.Command("v4l2-ctl", "--device="+device, "-D").Output()
	if err == nil {
		if m := cardTypeRe.FindSubmatch(out); m != nil {
			return strings.TrimSpace(string(m[1]))
		}
	}
	return filepath.Base(device)
}

// ListAudioDevices returns all ALSA capture devices reported by `arecord -l`.
func ListAudioDevices() []AudioDeviceInfo {
	out, err := exec.Command("arecord", "-l").Output()
	if err != nil {
		return nil
	}
	return parseAudioDevices(string(out))
}

// arecordRe matches lines like:
//
//	card 2: Device [USB Audio Device], device 0: USB Audio [USB Audio]
var arecordRe = regexp.MustCompile(`^card\s+(\d+):\s+[^\[]*\[([^\]]+)\],\s+device\s+(\d+):`)

func parseAudioDevices(output string) []AudioDeviceInfo {
	var devices []AudioDeviceInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		m := arecordRe.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		cardNum, _ := strconv.Atoi(m[1])
		devNum, _ := strconv.Atoi(m[3])
		devices = append(devices, AudioDeviceInfo{
			CardNum: cardNum,
			DevNum:  devNum,
			Name:    strings.TrimSpace(m[2]),
			Device:  fmt.Sprintf("hw:%d,%d", cardNum, devNum),
		})
	}
	return devices
}

// ProbeAudioCapabilities uses `arecord --dump-hw-params` (with a 2-second
// timeout) to retrieve the maximum supported sample rate and channel count for
// the given ALSA device. Returns (0, 0) when the probe fails or produces no
// parseable output.
func ProbeAudioCapabilities(device string) (maxRate int, maxChannels int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx,
		"arecord", "--dump-hw-params", "-D", device, "/dev/null",
	).CombinedOutput()
	return parseAudioHWParams(string(out))
}

var (
	// RATE: [8000 48000]
	hwRateRangeRe = regexp.MustCompile(`RATE:\s*\[(\d+)\s+(\d+)\]`)
	// RATE: 8000 11025 16000 44100 48000   or single  RATE: 48000
	hwRateListRe = regexp.MustCompile(`RATE:\s*((?:\d+\s*)+)`)
	// CHANNELS: [1 2]
	hwChanRangeRe = regexp.MustCompile(`CHANNELS:\s*\[(\d+)\s+(\d+)\]`)
	// CHANNELS: 2
	hwChanSingleRe = regexp.MustCompile(`CHANNELS:\s*(\d+)`)
)

func parseAudioHWParams(output string) (maxRate int, maxChannels int) {
	if m := hwRateRangeRe.FindStringSubmatch(output); m != nil {
		maxRate, _ = strconv.Atoi(m[2])
	} else if m := hwRateListRe.FindStringSubmatch(output); m != nil {
		for _, s := range strings.Fields(m[1]) {
			if r, err := strconv.Atoi(s); err == nil && r > maxRate {
				maxRate = r
			}
		}
	}

	if m := hwChanRangeRe.FindStringSubmatch(output); m != nil {
		maxChannels, _ = strconv.Atoi(m[2])
	} else if m := hwChanSingleRe.FindStringSubmatch(output); m != nil {
		maxChannels, _ = strconv.Atoi(m[1])
	}
	return
}

// SelectBestAudioRate returns the highest standard sample rate that satisfies
// both the device's native capability and the user-configured maximum.
// If the device capability is unknown (0), only configMaxRate is used.
func SelectBestAudioRate(deviceMaxRate, configMaxRate int) int {
	effective := configMaxRate
	if deviceMaxRate > 0 && deviceMaxRate < effective {
		effective = deviceMaxRate
	}
	standards := []int{192000, 96000, 48000, 44100, 32000, 22050, 16000, 8000}
	for _, r := range standards {
		if r <= effective {
			return r
		}
	}
	return 8000
}

// SelectBestAudioChannels returns the channel count not exceeding both the
// device capability and the user-configured maximum.
// If the device capability is unknown (0), configMaxCh is returned directly.
func SelectBestAudioChannels(deviceMaxCh, configMaxCh int) int {
	if deviceMaxCh <= 0 {
		return configMaxCh
	}
	if deviceMaxCh < configMaxCh {
		return deviceMaxCh
	}
	return configMaxCh
}

// ---------------------------------------------------------------------------
// UID-based device resolution
// ---------------------------------------------------------------------------

// udevSerialRe matches an "ID_SERIAL=..." line from udevadm output.
var udevSerialRe = regexp.MustCompile(`(?m)^ID_SERIAL=(.+)$`)

// udevSerial runs udevadm on the given device node and returns the ID_SERIAL
// property, or an empty string on failure.
func udevSerial(deviceNode string) string {
	out, err := exec.Command("udevadm", "info", "--query=property", "--name="+deviceNode).Output()
	if err != nil {
		return ""
	}
	if m := udevSerialRe.FindSubmatch(out); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// VideoDeviceUID returns the udev ID_SERIAL for the given /dev/videoN device.
// This value is stable across reboots and independent of the enumeration order.
// Returns an empty string when the information is unavailable (e.g. non-USB
// devices or when udevadm is not installed).
func VideoDeviceUID(device string) string {
	return udevSerial(device)
}

// videoByIDRe matches symlink names in /dev/v4l/by-id/:
//
//	usb-{uid}-video-index{N}
var videoByIDRe = regexp.MustCompile(`^usb-(.+)-video-index(\d+)$`)

// VideoDeviceByUID scans /dev/v4l/by-id/ for the primary capture node
// (index0) whose uid matches the given ID_SERIAL string (case-sensitive).
// Returns the resolved absolute path (e.g. /dev/video2) and true on success,
// or ("", false) when no matching symlink is found.
func VideoDeviceByUID(uid string) (string, bool) {
	entries, err := os.ReadDir("/dev/v4l/by-id")
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		m := videoByIDRe.FindStringSubmatch(e.Name())
		if m == nil || m[1] != uid || m[2] != "0" {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join("/dev/v4l/by-id", e.Name()))
		if err != nil {
			continue
		}
		return target, true
	}
	return "", false
}

// AudioDeviceUID returns the udev ID_SERIAL for ALSA card cardNum by querying
// the corresponding /dev/snd/controlCN device node.
// Returns an empty string when the information is unavailable.
func AudioDeviceUID(cardNum int) string {
	return udevSerial(fmt.Sprintf("/dev/snd/controlC%d", cardNum))
}

// audioByIDRe matches symlink names in /dev/snd/by-id/:
//
//	usb-{uid}-{NN}   (NN is the USB interface number, e.g. "02")
var audioByIDRe = regexp.MustCompile(`^usb-(.+)-\d{2}$`)

// controlCardRe extracts the card index from a controlCN path.
var controlCardRe = regexp.MustCompile(`controlC(\d+)$`)

// AudioCardByUID scans /dev/snd/by-id/ for the ALSA control device whose
// uid matches the given ID_SERIAL string.
// Returns the card number and true on success, or (-1, false) when not found.
func AudioCardByUID(uid string) (int, bool) {
	entries, err := os.ReadDir("/dev/snd/by-id")
	if err != nil {
		return -1, false
	}
	for _, e := range entries {
		m := audioByIDRe.FindStringSubmatch(e.Name())
		if m == nil || m[1] != uid {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join("/dev/snd/by-id", e.Name()))
		if err != nil {
			continue
		}
		cm := controlCardRe.FindStringSubmatch(target)
		if cm == nil {
			continue
		}
		n, _ := strconv.Atoi(cm[1])
		return n, true
	}
	return -1, false
}
