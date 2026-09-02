package logger

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const logsDir = "logs"

// tsLayout is RFC3339 with millisecond resolution, in local time — matching
// the console's local HH:MM:SS so file and console clocks agree during a
// live cross-reference session.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

var (
	mu       sync.Mutex
	logFile  *os.File // nil in console-only mode
	fileDate string   // "2006-01-02" of the currently open logFile
	runID    string   // set once, lazily, when Init opens the first log file

	// consoleOut is a dup of fd 1 saved before any GStreamer call, so logging
	// keeps working when fd 1 is redirected to /dev/null for Nvidia noise.
	consoleOut *os.File

	// issuesMu/issues back RecentIssues: a small ring buffer of the last few
	// warnings/errors, so a full-screen console UI (the dashboard) can show
	// "recent events" without owning a scrolling log view itself.
	issuesMu sync.Mutex
	issues   []Issue
)

// consoleEnabled gates the colored per-line console output written by write()
// below; the JSON file output is never affected by it. Defaults to enabled —
// only the dashboard (which takes over the terminal with its own full-screen
// redraw) turns it off, via SetConsoleOutput(false), for as long as it owns
// the screen.
var consoleEnabled atomic.Bool

func init() {
	consoleEnabled.Store(true)
}

// SetConsoleOutput enables or disables the colored per-line console output.
// The log file (when Init was called) is never affected. Used by the
// dashboard to silence scrolling log lines while it owns the terminal with
// its own full-screen redraw.
func SetConsoleOutput(enabled bool) {
	consoleEnabled.Store(enabled)
}

// maxIssues caps the RecentIssues ring buffer.
const maxIssues = 8

// Issue is one recent warning/error, for display in the dashboard's "recent
// events" panel.
type Issue struct {
	Time      time.Time
	Level     string // "WARN", "ERROR" or "FATAL"
	Component string
	Instance  string
	Msg       string
}

func recordIssue(level, component, instance, msg string) {
	issuesMu.Lock()
	defer issuesMu.Unlock()
	issues = append(issues, Issue{
		Time: time.Now(), Level: strings.TrimSpace(level),
		Component: component, Instance: instance, Msg: msg,
	})
	if len(issues) > maxIssues {
		issues = issues[len(issues)-maxIssues:]
	}
}

// RecentIssues returns the last few warnings/errors logged, oldest first.
func RecentIssues() []Issue {
	issuesMu.Lock()
	defer issuesMu.Unlock()
	out := make([]Issue, len(issues))
	copy(out, issues)
	return out
}

// entry is one JSON Lines record written to the log file. Console output is
// unaffected by this — it stays the colored, human-readable text format.
type entry struct {
	TS        string         `json:"ts"`
	Level     string         `json:"level"`
	Component string         `json:"component,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	Msg       string         `json:"msg"`
	RunID     string         `json:"run_id"`
	Fields    map[string]any `json:"fields,omitempty"`
}

func init() {
	// dup(1) before any dup2(/dev/null, 1) calls so consoleOut always points to the terminal.
	if fd, err := syscall.Dup(int(os.Stdout.Fd())); err == nil {
		syscall.CloseOnExec(fd) // do not inherit in child processes
		consoleOut = os.NewFile(uintptr(fd), "stdout")
	} else {
		consoleOut = os.Stdout
	}
}

// ConsoleOut returns the console's underlying file (a dup of the original
// fd 1, unaffected by GStreamer redirecting fd 1 to /dev/null). Used by the
// dashboard to draw directly to the real terminal.
func ConsoleOut() *os.File {
	return consoleOut
}

// Init initializes the logger with a daily-rotated JSON-Lines file under
// logs/<date>.log, in addition to the console, and returns a close
// function. The file rolls over to the next day's automatically on the
// first write after midnight — a long-running process (e.g. spanning an
// overnight rally) doesn't need to be restarted for logs to stay split by
// day. Each line also carries a run_id unique to this process start, so a
// day's file spanning several restarts can be sliced to just one run.
func Init() func() {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		log.Fatalf("failed to create log directory: %v", err)
	}
	mu.Lock()
	runID = strconv.FormatInt(time.Now().UnixNano(), 36)
	openTodayLocked()
	path := logFile.Name()
	mu.Unlock()

	Info("Logging to %s", path)
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if logFile != nil {
			logFile.Close()
			logFile = nil
		}
	}
}

// openTodayLocked (re)opens logs/<today>.log if the date has changed since
// the currently open file (or none is open yet). mu must be held.
func openTodayLocked() {
	today := time.Now().Format("2006-01-02")
	if logFile != nil {
		if fileDate == today {
			return
		}
		logFile.Close()
	}
	logPath := filepath.Join(logsDir, today+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open log file %s: %v", logPath, err)
	}
	logFile = f
	fileDate = today
}

// splitPrefix extracts a leading "[component]" or "[component:instance]" tag
// from an already-formatted message (called after Sprintf, not on the
// format string itself — so a "[...]"-looking substring anywhere but the
// very start, e.g. inside a quoted kernel-log line, never matches). Returns
// comp="", inst="", rest=msg unchanged if msg doesn't start with such a tag.
func splitPrefix(msg string) (comp, inst, rest string) {
	if !strings.HasPrefix(msg, "[") {
		return "", "", msg
	}
	end := strings.IndexByte(msg, ']')
	if end <= 0 {
		return "", "", msg
	}
	comp, inst = splitComponent(msg[1:end])
	return comp, inst, strings.TrimPrefix(msg[end+1:], " ")
}

// splitComponent splits "component" or "component:instance" on the first
// colon only — instance may itself contain further colons (e.g. pipeline
// labels like "camera:front-cam:source" -> component "camera", instance
// "front-cam:source").
func splitComponent(raw string) (comp, inst string) {
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// write renders one log line to the console (unchanged text format,
// reconstructing the "[component]"/"[component:instance]" tag so console
// output is unaffected by this file's JSON change) and, if a log file is
// open, appends one JSON Lines record.
func write(level, ansiLevel, component, instance, msg string, fields map[string]any) {
	now := time.Now()

	consoleMsg := msg
	if component != "" {
		tag := "[" + component
		if instance != "" {
			tag += ":" + instance
		}
		consoleMsg = tag + "] " + msg
	}

	trimmed := strings.TrimSpace(level)
	if trimmed == "WARN" || trimmed == "ERROR" || trimmed == "FATAL" {
		recordIssue(level, component, instance, msg)
	}

	mu.Lock()
	defer mu.Unlock()
	if consoleEnabled.Load() {
		fmt.Fprintf(consoleOut, "%s | %s%s\033[0m %s\n", now.Format("15:04:05"), ansiLevel, level, consoleMsg)
	}

	if logFile != nil {
		openTodayLocked() // no-op unless the date has rolled over since the last write
		e := entry{
			TS:        now.Format(tsLayout),
			Level:     strings.ToLower(strings.TrimSpace(level)),
			Component: component,
			Instance:  instance,
			Msg:       msg,
			RunID:     runID,
			Fields:    fields,
		}
		line, err := json.Marshal(e)
		if err != nil {
			// Only reachable if a Fields value isn't marshalable (channel, func,
			// cycle) — none of today's call sites do that, but fall back to a
			// guaranteed-valid line rather than dropping or corrupting the file.
			line, _ = json.Marshal(entry{
				TS: now.Format(tsLayout), Level: "error", Component: "logger",
				Msg: "failed to marshal log entry: " + err.Error(), RunID: runID,
			})
		}
		logFile.Write(line)
		logFile.Write([]byte("\n"))
	}
}

func Info(format string, v ...any) {
	comp, inst, msg := splitPrefix(fmt.Sprintf(format, v...))
	write("INFO ", "\033[36m", comp, inst, msg, nil)
}

func Warn(format string, v ...any) {
	comp, inst, msg := splitPrefix(fmt.Sprintf(format, v...))
	write("WARN ", "\033[33m", comp, inst, msg, nil)
}

func Error(format string, v ...any) {
	comp, inst, msg := splitPrefix(fmt.Sprintf(format, v...))
	write("ERROR", "\033[31m", comp, inst, msg, nil)
}

func Fatal(format string, v ...any) {
	comp, inst, msg := splitPrefix(fmt.Sprintf(format, v...))
	write("FATAL", "\033[1;31m", comp, inst, msg, nil)
	os.Exit(1)
}

// InfoFields, WarnFields and ErrorFields log msg (already formatted by the
// caller) tagged with component (accepting the same "component" or
// "component:instance" shorthand used in Info/Warn/Error's bracket
// convention) plus arbitrary structured fields, nested under "fields" in
// the JSON line (never flattened into the envelope — a fields key
// accidentally named e.g. "msg" or "level" would otherwise silently
// corrupt it). Reserved for the handful of call sites whose data is worth
// querying directly (bitrate/loss numbers, state transitions, watchdog
// actions) rather than left inside a formatted message string.
func InfoFields(component, msg string, fields map[string]any) {
	comp, inst := splitComponent(component)
	write("INFO ", "\033[36m", comp, inst, msg, fields)
}

func WarnFields(component, msg string, fields map[string]any) {
	comp, inst := splitComponent(component)
	write("WARN ", "\033[33m", comp, inst, msg, fields)
}

func ErrorFields(component, msg string, fields map[string]any) {
	comp, inst := splitComponent(component)
	write("ERROR", "\033[31m", comp, inst, msg, fields)
}
