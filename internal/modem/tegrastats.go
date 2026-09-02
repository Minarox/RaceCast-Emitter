package modem

// tegrastats.go folds modem_watch.sh's other diagnostic capture into the
// app: a one-shot tegrastats snapshot (RAM/CPU/thermal/power draw) taken
// around modem fault events, for correlating a dropout with power/thermal
// conditions — the same signal that helped diagnose the original USB power
// instability. Triggered from handleMMSignal in modem.go: on the modem
// object disappearing, reappearing, and entering the "failed" state.

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"time"

	"racecast-emitter/internal/logger"
)

// logTegrastats runs tegrastatsSnapshot and logs the result (or the
// failure) tagged with reason, identifying which event triggered it.
// Best-effort and backgrounded by its callers — never on the D-Bus
// signal-handling goroutine's critical path.
func logTegrastats(reason string) {
	line, err := tegrastatsSnapshot(context.Background())
	if err != nil {
		logger.WarnFields("modem", "tegrastats snapshot failed", map[string]any{"reason": reason, "error": err.Error()})
		return
	}
	logger.InfoFields("modem", "tegrastats snapshot", map[string]any{"reason": reason, "raw": line})
}

// tegrastatsSnapshot runs `tegrastats` just long enough to capture one
// line of output, then kills it — tegrastats otherwise keeps printing a new
// line every interval indefinitely. Same approach as modem_watch.sh's
// `timeout 2 tegrastats --interval 1 | head -1`.
func tegrastatsSnapshot(ctx context.Context) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "tegrastats", "--interval", "1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(stdout)
	var line string
	if scanner.Scan() {
		line = scanner.Text()
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	if line == "" {
		return "", fmt.Errorf("no tegrastats output")
	}
	return line, nil
}
