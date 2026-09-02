// Package dashboard renders a full-screen, auto-refreshing console status
// view for RaceCast-Emitter's normal run mode (record/stream), replacing the
// scrolling log output on screen with a live snapshot of every subsystem.
// The log file is unaffected — this is purely a console presentation layer
// on top of state the rest of the program already exposes (modem.GetStatus,
// pipeline.Slot, telemetry.Conn, logger.RecentIssues).
package dashboard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"racecast-emitter/internal/config"
	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/modem"
	"racecast-emitter/internal/pipeline"
	"racecast-emitter/internal/telemetry"
)

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiRed     = "\033[31m"
	ansiCyan    = "\033[36m"
	ansiGray    = "\033[90m"
	refreshRate = 1 * time.Second
)

// Deps is the state the dashboard reads every tick. All fields are read-only
// from the dashboard's point of view — it never mutates program state.
type Deps struct {
	StartTime   time.Time
	Cfg         *config.Config
	CameraSlots map[string]*pipeline.Slot
	MicSlots    map[string]*pipeline.Slot
	TelemConn   *telemetry.Conn
	DoRecord    bool
	DoStream    bool
}

// Run takes over the terminal with the full-screen status view, redrawing in
// place every refreshRate until ctx is cancelled, then restores the normal
// scrolling terminal. Regular console log lines (logger.Info/Warn/...) are
// suppressed for the duration — recent warnings/errors are surfaced in the
// dashboard's own "Derniers événements" panel instead — but the JSON log
// file keeps recording everything, unaffected.
func Run(ctx context.Context, deps Deps) {
	out := logger.ConsoleOut()

	logger.SetConsoleOutput(false)
	fmt.Fprint(out, "\033[?1049h\033[?25l\033[2J") // alt screen, hide cursor, clear
	defer func() {
		fmt.Fprint(out, "\033[?25h\033[?1049l") // show cursor, leave alt screen
		logger.SetConsoleOutput(true)
	}()

	render(out, deps)

	ticker := time.NewTicker(refreshRate)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			render(out, deps)
		}
	}
}

// render draws one full frame: cursor home, one line per status row (each
// cleared to end-of-line so a shorter value never leaves stale characters
// behind it), then clear-to-end-of-screen (in case this frame has fewer
// lines than the previous one, e.g. the "Derniers événements" panel shrank).
func render(out *os.File, deps Deps) {
	var b strings.Builder
	b.WriteString("\033[H")

	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\033[K\n")
	}
	blank := func() { line("") }
	label := func(text string) string { return ansiBold + text + ansiReset }

	line("%s--=[ RaceCast ]=--%s", ansiBold, ansiReset)
	blank()
	line("* %s : %s", label("Programme lancé depuis"), formatUptime(time.Since(deps.StartTime)))
	line("* %s : %s", label("Modem"), modemLine())
	line("* %s : %s", label("GPS"), gpsLine())
	line("* %s : %s", label("Caméras"), deviceLine(collectCameras(deps.Cfg, deps.CameraSlots)))
	line("* %s : %s", label("Microphones"), deviceLine(collectMicrophones(deps.Cfg, deps.MicSlots)))
	line("* %s : %s", label("Télémétrie"), telemetryLine(deps.TelemConn, deps.DoStream))
	line("* %s : %s", label("Enregistrement"), fluxLine(deps.DoRecord, "mode diffusion uniquement", func(s *pipeline.Slot) bool { return s.IsRecordRunning() }, deps.CameraSlots, deps.MicSlots))
	line("* %s : %s", label("Diffusion"), fluxLine(deps.DoStream, "mode enregistrement uniquement", func(s *pipeline.Slot) bool { return s.IsStreamRunning() }, deps.CameraSlots, deps.MicSlots))
	blank()
	line("%s", label("Derniers événements"))
	writeIssues(line)

	b.WriteString("\033[J")
	fmt.Fprint(out, b.String())
}

func formatUptime(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func modemLine() string {
	st := modem.GetStatus()
	if !st.Detected {
		return ansiRed + "Non détecté" + ansiReset
	}

	var state, color string
	switch {
	case st.Recovering:
		state, color = "Récupération en cours...", ansiYellow
	case !st.ConnKnown:
		state, color = "Initialisation...", ansiYellow
	case st.Connected:
		state, color = "Connecté", ansiGreen
	default:
		state, color = "Déconnecté", ansiRed
	}

	tech := "?"
	if st.Tech != "" {
		tech = strings.ToUpper(st.Tech)
	}
	signal := "signal inconnu"
	if st.HasStats {
		signal = fmt.Sprintf("signal %d%%", st.Quality)
	}
	return fmt.Sprintf("%s%s%s (%s, %s)", color, state, ansiReset, tech, signal)
}

func gpsLine() string {
	if modem.NMEAAge() >= time.Hour {
		return ansiGray + "Aucune donnée reçue" + ansiReset
	}
	sentences, _ := modem.GetNMEA()
	pos, hasPos := modem.ParseNMEA(sentences)
	if !hasPos {
		return ansiGray + "Aucune donnée reçue" + ansiReset
	}
	if !pos.Fix {
		return fmt.Sprintf("%sPas de fix%s (%d satellite(s))", ansiRed, ansiReset, pos.Sats)
	}
	precision, pcolor := hdopLabel(pos.HDOP)
	return fmt.Sprintf("%sFix%s (%s%s%s, %d satellite(s))", ansiGreen, ansiReset, pcolor, precision, ansiReset, pos.Sats)
}

func hdopLabel(hdop float64) (string, string) {
	switch {
	case hdop <= 0:
		return "précision inconnue", ansiGray
	case hdop < 2:
		return fmt.Sprintf("excellente précision, HDOP %.1f", hdop), ansiGreen
	case hdop < 5:
		return fmt.Sprintf("bonne précision, HDOP %.1f", hdop), ansiGreen
	case hdop < 10:
		return fmt.Sprintf("précision moyenne, HDOP %.1f", hdop), ansiYellow
	default:
		return fmt.Sprintf("précision faible, HDOP %.1f", hdop), ansiRed
	}
}

// namedDevice pairs a display name with its Slot (nil if not currently
// active — disabled in config, or excluded because neither recording nor
// streaming applies to it).
type namedDevice struct {
	name string
	slot *pipeline.Slot
}

func collectCameras(cfg *config.Config, slots map[string]*pipeline.Slot) []namedDevice {
	var out []namedDevice
	for _, c := range cfg.Cameras {
		if c.Disabled {
			continue
		}
		if s, ok := slots[c.UID]; ok {
			out = append(out, namedDevice{c.Name, s})
		}
	}
	return out
}

func collectMicrophones(cfg *config.Config, slots map[string]*pipeline.Slot) []namedDevice {
	var out []namedDevice
	for _, m := range cfg.Microphones {
		if m.Disabled {
			continue
		}
		if s, ok := slots[m.UID]; ok {
			out = append(out, namedDevice{m.Name, s})
		}
	}
	return out
}

func deviceLine(devices []namedDevice) string {
	if len(devices) == 0 {
		return ansiGray + "0" + ansiReset
	}
	parts := make([]string, len(devices))
	for i, d := range devices {
		parts[i] = colorDeviceName(d.name, d.slot)
	}
	return fmt.Sprintf("%d (%s)", len(devices), strings.Join(parts, ", "))
}

// colorDeviceName reflects each device's live pipeline state: red if its
// source isn't running (device disconnected/not found), green if both
// record and stream are active, cyan if only one of the two is, yellow if
// the source is up but neither consumer is (e.g. briefly during startup).
func colorDeviceName(name string, s *pipeline.Slot) string {
	if s == nil || !s.IsSourceRunning() {
		return ansiRed + name + ansiReset
	}
	rec, str := s.IsRecordRunning(), s.IsStreamRunning()
	switch {
	case rec && str:
		return ansiGreen + name + ansiReset
	case rec || str:
		return ansiCyan + name + ansiReset
	default:
		return ansiYellow + name + ansiReset
	}
}

func telemetryLine(conn *telemetry.Conn, doStream bool) string {
	if conn == nil || !conn.Configured() {
		return ansiGray + "Désactivée (RC_SRT_HOST non défini)" + ansiReset
	}
	if !doStream {
		return ansiGray + "Désactivée (mode enregistrement uniquement)" + ansiReset
	}
	if conn.IsConnected() {
		return ansiGreen + "Connectée" + ansiReset
	}
	return ansiRed + "Déconnectée" + ansiReset
}

// fluxLine summarizes how many of the camera+microphone slots currently
// satisfy check (recording or streaming) out of the total configured, or
// reports the section as disabled when enabled is false (record-only /
// stream-only launch modes).
func fluxLine(enabled bool, disabledReason string, check func(*pipeline.Slot) bool, cameraSlots, micSlots map[string]*pipeline.Slot) string {
	if !enabled {
		return ansiGray + "Désactivé (" + disabledReason + ")" + ansiReset
	}
	total, active := 0, 0
	for _, s := range cameraSlots {
		total++
		if check(s) {
			active++
		}
	}
	for _, s := range micSlots {
		total++
		if check(s) {
			active++
		}
	}
	if total == 0 {
		return ansiGray + "Aucun flux configuré" + ansiReset
	}
	switch {
	case active == total:
		return fmt.Sprintf("%sActif%s (%d/%d flux)", ansiGreen, ansiReset, active, total)
	case active == 0:
		return fmt.Sprintf("%sInactif%s (%d/%d flux)", ansiRed, ansiReset, active, total)
	default:
		return fmt.Sprintf("%sPartiel%s (%d/%d flux)", ansiYellow, ansiReset, active, total)
	}
}

// writeIssues appends the recent-warnings/errors panel via the same line
// helper render() uses, most recent first, or a gray placeholder if none.
func writeIssues(line func(format string, args ...any)) {
	issues := logger.RecentIssues()
	if len(issues) == 0 {
		line("  %sAucun%s", ansiGray, ansiReset)
		return
	}
	for i := len(issues) - 1; i >= 0; i-- {
		it := issues[i]
		color := ansiYellow
		if it.Level == "ERROR" || it.Level == "FATAL" {
			color = ansiRed
		}
		tag := it.Component
		if it.Instance != "" {
			tag += ":" + it.Instance
		}
		if tag != "" {
			tag = "[" + tag + "] "
		}
		line("  %s %s%-5s%s %s%s", it.Time.Format("15:04:05"), color, it.Level, ansiReset, tag, it.Msg)
	}
}
