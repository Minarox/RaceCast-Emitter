package devices

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// udevSerial retourne la valeur de ID_SERIAL pour un nœud de périphérique.
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

// FindVideo retourne le chemin /dev/videoX correspondant à l'UID donné.
func FindVideo(uid string) (string, error) {
	// Tentative via /dev/v4l/by-id/ (symlinks par identifiant)
	const byIDPath = "/dev/v4l/by-id"
	if entries, err := os.ReadDir(byIDPath); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.Contains(name, uid) {
				continue
			}
			// Ignorer les index secondaires (métadonnées)
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

	// Repli : parcourir /dev/video* avec udevadm
	matches, _ := filepath.Glob("/dev/video*")
	for _, dev := range matches {
		if serial, ok := udevSerial(dev); ok && serial == uid {
			return dev, nil
		}
	}

	return "", fmt.Errorf("périphérique vidéo avec UID %q introuvable", uid)
}

// FindALSA retourne la chaîne hw:C,D correspondant à l'UID donné.
func FindALSA(uid string) (string, error) {
	// Tentative via /dev/snd/by-id/ : les symlinks pointent vers controlCX.
	// On en déduit le numéro de carte, puis on sélectionne le premier nœud
	// de capture pcmC{X}D0c (device 0 = micro USB).
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
			// link ressemble à "../controlC2"
			base := filepath.Base(link) // "controlC2"
			var card int
			if _, err := fmt.Sscanf(base, "controlC%d", &card); err != nil {
				continue
			}
			return fmt.Sprintf("hw:%d,0", card), nil
		}
	}

	// Repli : parcourir /dev/snd/pcmC*D*c avec udevadm
	matches, _ := filepath.Glob("/dev/snd/pcmC*D*c")
	for _, dev := range matches {
		if serial, ok := udevSerial(dev); ok && serial == uid {
			var card, device int
			base := filepath.Base(dev) // ex. "pcmC1D0c"
			fmt.Sscanf(base, "pcmC%dD%dc", &card, &device)
			return fmt.Sprintf("hw:%d,%d", card, device), nil
		}
	}
	return "", fmt.Errorf("périphérique ALSA avec UID %q introuvable", uid)
}
