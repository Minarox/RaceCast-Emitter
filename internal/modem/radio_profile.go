package modem

// radio_profile.go applies a configured radio mode/band profile every time
// the modem (re)attaches — initial startup, a USB re-enumeration, or the
// health watchdog's own Modem.Reset — so the preference survives all of
// those instead of only holding until the next reconnect. ModemManager does
// not persist SetCurrentModes/SetCurrentBands to the modem's NV storage;
// re-applying on every attachModem() call is what makes it stick.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"racecast-emitter/internal/logger"
)

// applyRadioProfile sets the allowed/preferred network modes (and, if
// configured, a band lock) via mmcli rather than calling ModemManager's
// SetCurrentModes/SetCurrentBands D-Bus methods directly: those take raw
// MMModemMode/MMModemBand bitmask integers, and a wrong value from memory
// would silently misconfigure the radio. mmcli resolves the same
// human-readable names `mmcli -m any` already prints ("4g", "eutran-20", …),
// so there's no bitmask to get wrong. Best-effort: failures are logged, not
// fatal to attachModem — the modem keeps working with its existing/default
// settings either way.
func applyRadioProfile(ctx context.Context) {
	allowed := strings.TrimSpace(os.Getenv("RC_MODEM_ALLOWED_MODES"))
	if allowed == "" {
		allowed = "3g|4g|5g"
	}
	preferred := strings.TrimSpace(os.Getenv("RC_MODEM_PREFERRED_MODE"))
	if preferred == "" {
		preferred = "4g"
	}

	modeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	out, err := exec.CommandContext(modeCtx, "mmcli", "-m", "any",
		"--set-allowed-modes="+allowed,
		"--set-preferred-mode="+preferred,
	).CombinedOutput()
	cancel()
	if err != nil {
		logger.Warn("[modem] Failed to apply mode profile (allowed=%s preferred=%s): %v (%s)",
			allowed, preferred, err, strings.TrimSpace(string(out)))
	} else {
		logger.Info("[modem] Radio mode profile applied: allowed=%s preferred=%s", allowed, preferred)
	}

	// No restriction by default (see .env.example): a bad band lock can leave
	// the modem with zero service instead of falling back, if the locked
	// bands aren't actually deployed at a given stage.
	bands := strings.TrimSpace(os.Getenv("RC_MODEM_BANDS"))
	if bands == "" {
		return
	}

	bandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	out, err = exec.CommandContext(bandCtx, "mmcli", "-m", "any", "--set-current-bands="+bands).CombinedOutput()
	cancel()
	if err != nil {
		logger.Warn("[modem] Failed to apply band lock (%s): %v (%s)", bands, err, strings.TrimSpace(string(out)))
	} else {
		logger.Info("[modem] Band lock applied: %s", bands)
	}
}
