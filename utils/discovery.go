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

// defaultAudioExcludePatterns is a built-in list of card-name substrings that
// identify known internal or virtual ALSA devices that are never real capture
// sources. Matching is case-insensitive.
//
// "NVIDIA Jetson" covers the Tegra APE Audio Processing Engine which exposes
// up to 20 virtual XBAR-ADMAIF nodes but carries no actual microphone signal.
var defaultAudioExcludePatterns = []string{
	"NVIDIA Jetson",
}

// audioExcludePatterns returns the merged list of built-in and user-configured
// exclusion patterns. The env var MIC_EXCLUDE accepts a comma-separated list of
// additional case-insensitive substrings to exclude by card name.
//
// Example:  MIC_EXCLUDE="HDMI,HDA Intel"
func audioExcludePatterns() []string {
	patterns := make([]string, len(defaultAudioExcludePatterns))
	copy(patterns, defaultAudioExcludePatterns)

	if env := os.Getenv("MIC_EXCLUDE"); env != "" {
		for _, p := range strings.Split(env, ",") {
			if t := strings.TrimSpace(p); t != "" {
				patterns = append(patterns, t)
			}
		}
	}
	return patterns
}

// isExcludedAudioDevice returns true when the card name matches any exclusion
// pattern (case-insensitive substring).
func isExcludedAudioDevice(cardName string, patterns []string) bool {
	lower := strings.ToLower(cardName)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// videoExcludePatterns returns the list of user-configured exclusion patterns
// for video devices. The env var CAM_EXCLUDE accepts a comma-separated list of
// case-insensitive substrings matched against the V4L2 card name.
//
// Example:  CAM_EXCLUDE="bcm2835,Dummy"
func videoExcludePatterns() []string {
	var patterns []string
	if env := os.Getenv("CAM_EXCLUDE"); env != "" {
		for _, p := range strings.Split(env, ",") {
			if t := strings.TrimSpace(p); t != "" {
				patterns = append(patterns, t)
			}
		}
	}
	return patterns
}

// isExcludedVideoDevice returns true when the card name matches any exclusion
// pattern (case-insensitive substring).
func isExcludedVideoDevice(cardName string, patterns []string) bool {
	lower := strings.ToLower(cardName)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// VideoDeviceMode represents a supported resolution+framerate combination for a V4L2 device.
type VideoDeviceMode struct {
	Width     int
	Height    int
	Framerate int
	Format    CameraFormat
}

// AudioDeviceInfo represents a discovered ALSA capture device.
type AudioDeviceInfo struct {
	CardNum int
	DevNum  int
	Name    string
	Device  string // e.g. "hw:2,0"
}

// ListVideoDevices returns the /dev/videoN paths that are valid V4L2 capture
// devices (i.e. they respond to --list-formats with at least one format entry).
// Metadata-only nodes, non-video entries, and devices whose card name matches
// the CAM_EXCLUDE patterns are skipped automatically.
func ListVideoDevices() []string {
	matches, _ := filepath.Glob("/dev/video*")
	exclude := videoExcludePatterns()
	var result []string
	for _, m := range matches {
		out, err := exec.Command("v4l2-ctl", "--device="+m, "--list-formats").Output()
		if err != nil || !strings.Contains(string(out), "[") {
			continue
		}
		if len(exclude) > 0 {
			name := GetVideoDeviceName(m)
			if isExcludedVideoDevice(name, exclude) {
				Log.Infow("Video device excluded by filter.", "device", m, "name", name)
				continue
			}
		}
		result = append(result, m)
	}
	return result
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

// ListVideoModes returns all MJPEG and YUYV modes supported by a V4L2 device.
func ListVideoModes(device string) ([]VideoDeviceMode, error) {
	out, err := exec.Command("v4l2-ctl", "--device="+device, "--list-formats-ext").Output()
	if err != nil {
		return nil, fmt.Errorf("v4l2-ctl failed for %s: %w", device, err)
	}
	return parseVideoModes(string(out)), nil
}

var (
	vidFormatRe = regexp.MustCompile(`'(MJPG|YUYV)'`)
	vidSizeRe   = regexp.MustCompile(`Size:\s+Discrete\s+(\d+)x(\d+)`)
	vidFPSRe    = regexp.MustCompile(`\((\d+(?:\.\d+)?)\s+fps\)`)
)

func parseVideoModes(output string) []VideoDeviceMode {
	var modes []VideoDeviceMode
	var currentFormat CameraFormat
	var currentW, currentH int

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if m := vidFormatRe.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "MJPG":
				currentFormat = FormatMJPEG
			case "YUYV":
				currentFormat = FormatYUYV
			default:
				currentFormat = ""
			}
			currentW, currentH = 0, 0
		} else if m := vidSizeRe.FindStringSubmatch(line); m != nil {
			currentW, _ = strconv.Atoi(m[1])
			currentH, _ = strconv.Atoi(m[2])
		} else if m := vidFPSRe.FindStringSubmatch(line); m != nil && currentFormat != "" {
			fps, _ := strconv.ParseFloat(m[1], 64)
			if currentW > 0 && currentH > 0 && fps > 0 {
				modes = append(modes, VideoDeviceMode{
					Width:     currentW,
					Height:    currentH,
					Framerate: int(fps),
					Format:    currentFormat,
				})
			}
		}
	}
	return modes
}

// SelectBestVideoMode picks the highest-quality mode that does not exceed
// maxW×maxH at maxFPS. MJPEG is preferred over YUYV. Within the same format,
// the largest frame area wins; ties are broken by higher framerate.
// Returns (mode, true) if at least one valid mode exists.
func SelectBestVideoMode(modes []VideoDeviceMode, maxW, maxH, maxFPS int) (VideoDeviceMode, bool) {
	var best VideoDeviceMode
	found := false
	for _, m := range modes {
		if m.Width > maxW || m.Height > maxH || m.Framerate > maxFPS {
			continue
		}
		if !found {
			best = m
			found = true
			continue
		}
		// Prefer MJPEG
		if m.Format == FormatMJPEG && best.Format != FormatMJPEG {
			best = m
			continue
		}
		if m.Format != FormatMJPEG && best.Format == FormatMJPEG {
			continue
		}
		// Same format: largest pixel count first, then highest FPS
		if m.Width*m.Height > best.Width*best.Height ||
			(m.Width*m.Height == best.Width*best.Height && m.Framerate > best.Framerate) {
			best = m
		}
	}
	return best, found
}

// ListAudioDevices returns all ALSA capture devices reported by `arecord -l`,
// excluding internal/virtual cards (e.g. Tegra APE) based on built-in patterns
// and the MIC_EXCLUDE environment variable.
func ListAudioDevices() []AudioDeviceInfo {
	out, err := exec.Command("arecord", "-l").Output()
	if err != nil {
		return nil
	}
	exclude := audioExcludePatterns()
	var result []AudioDeviceInfo
	for _, dev := range parseAudioDevices(string(out)) {
		if isExcludedAudioDevice(dev.Name, exclude) {
			Log.Infow("Audio device excluded by filter.", "device", dev.Device, "name", dev.Name)
			continue
		}
		result = append(result, dev)
	}
	return result
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
