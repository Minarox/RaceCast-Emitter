package devices

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// udevSerial returns the ID_SERIAL value for a device node.
func udevSerial(dev string) (string, bool) {
	out, err := exec.Command("udevadm", "info", "--query=property", "--name="+dev).Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, "ID_SERIAL="); ok {
			return strings.TrimSpace(after), true
		}
	}
	return "", false
}

// FindVideo returns the /dev/videoX path matching the given UID.
func FindVideo(uid string) (string, error) {
	// Try /dev/v4l/by-id/ symlinks first.
	const byIDPath = "/dev/v4l/by-id"
	if entries, err := os.ReadDir(byIDPath); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.Contains(name, uid) {
				continue
			}
			// Skip secondary indexes (metadata nodes).
			if strings.Contains(name, "index") && !strings.Contains(name, "index0") {
				continue
			}
			link, err := os.Readlink(filepath.Join(byIDPath, name))
			if err != nil {
				continue
			}
			if !filepath.IsAbs(link) {
				link = filepath.Clean(filepath.Join(byIDPath, link))
			}
			return link, nil
		}
	}

	// Fallback: scan /dev/video* via udevadm.
	matches, _ := filepath.Glob("/dev/video*")
	for _, dev := range matches {
		if serial, ok := udevSerial(dev); ok && serial == uid {
			return dev, nil
		}
	}

	return "", fmt.Errorf("video device with UID %q not found", uid)
}

// FindALSA returns the hw:C,D string matching the given UID.
func FindALSA(uid string) (string, error) {
	// Try /dev/snd/by-id/ symlinks: they point to controlCX nodes.
	// Derive the card number, then select the first capture node pcmC{X}D0c.
	const byIDPath = "/dev/snd/by-id"
	if entries, err := os.ReadDir(byIDPath); err == nil {
		for _, e := range entries {
			if !strings.Contains(e.Name(), uid) {
				continue
			}
			link, err := os.Readlink(filepath.Join(byIDPath, e.Name()))
			if err != nil {
				continue
			}
			base := filepath.Base(link) // e.g. "controlC2"
			var card int
			if _, err := fmt.Sscanf(base, "controlC%d", &card); err != nil {
				continue
			}
			return fmt.Sprintf("hw:%d,0", card), nil
		}
	}

	// Fallback: scan /dev/snd/pcmC*D*c via udevadm.
	matches, _ := filepath.Glob("/dev/snd/pcmC*D*c")
	for _, dev := range matches {
		if serial, ok := udevSerial(dev); ok && serial == uid {
			var card, device int
			base := filepath.Base(dev) // e.g. "pcmC1D0c"
			if _, err := fmt.Sscanf(base, "pcmC%dD%dc", &card, &device); err != nil {
				continue
			}
			return fmt.Sprintf("hw:%d,%d", card, device), nil
		}
	}
	return "", fmt.Errorf("ALSA device with UID %q not found", uid)
}
