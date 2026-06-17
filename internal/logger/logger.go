package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const logsDir = "logs"

var (
	mu      sync.Mutex
	logFile *os.File

	// consoleOut est un *os.File ouvert sur un duplicata de fd 1 créé au démarrage
	// du programme, avant tout appel GStreamer. Quand le package pipeline redirige
	// temporairement fd 1 vers /dev/null (pour masquer les messages des drivers
	// Nvidia), consoleOut continue d'écrire sur le terminal d'origine.
	consoleOut *os.File

	infoFn  func(string, ...any)
	warnFn  func(string, ...any)
	errorFn func(string, ...any)
	fatalFn func(string, ...any)
)

func init() {
	// dup(1) crée un nouveau descripteur pointant vers le même terminal que fd 1.
	// Ce descripteur n'est pas affecté par dup2(/dev/null, 1) appelé plus tard.
	if fd, err := syscall.Dup(int(os.Stdout.Fd())); err == nil {
		syscall.CloseOnExec(fd) // ne pas hériter dans les processus fils
		consoleOut = os.NewFile(uintptr(fd), "stdout")
	} else {
		consoleOut = os.Stdout
	}
}

// InitConsole initialise le logger en mode console uniquement (pas de fichier).
// À utiliser pour les commandes interactives comme --ups.
func InitConsole() {
	setup(nil)
}

// Init initialise le logger et retourne une fonction de fermeture du fichier.
func Init() func() {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		log.Fatalf("Impossible de créer le dossier de logs : %v", err)
	}

	logPath := filepath.Join(logsDir, time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("Impossible d'ouvrir le fichier de log %s : %v", logPath, err)
	}

	setup(f)
	Info("Logs enregistrés dans %s", logPath)
	return func() { f.Close() }
}

func setup(f *os.File) {
	logFile = f

	write := func(out *os.File, level, ansiLevel, msg string) {
		now := time.Now()
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(out, "%s | %s%s\033[0m %s\n", now.Format("15:04:05"), ansiLevel, level, msg)
		if f != nil {
			fmt.Fprintf(f, "%s | %s %s\n", now.Format("2006/01/02 15:04:05"), level, msg)
		}
	}

	infoFn = func(format string, v ...any) {
		write(consoleOut, "INFO ", "\033[36m", fmt.Sprintf(format, v...))
	}
	warnFn = func(format string, v ...any) {
		write(consoleOut, "WARN ", "\033[33m", fmt.Sprintf(format, v...))
	}
	errorFn = func(format string, v ...any) {
		write(consoleOut, "ERROR", "\033[31m", fmt.Sprintf(format, v...))
	}
	fatalFn = func(format string, v ...any) {
		write(consoleOut, "FATAL", "\033[1;31m", fmt.Sprintf(format, v...))
		os.Exit(1)
	}
}

func Info(format string, v ...any)  { infoFn(format, v...) }
func Warn(format string, v ...any)  { warnFn(format, v...) }
func Error(format string, v ...any) { errorFn(format, v...) }
func Fatal(format string, v ...any) { fatalFn(format, v...) }

