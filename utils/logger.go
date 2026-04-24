package utils

import (
	"log"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	lock = &sync.Mutex{}
	config zap.Config = getConfig()
	Log *zap.SugaredLogger
)

func getConfig() zap.Config {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{
		"stdout",
		"logs/" + time.Now().Format(time.DateOnly) + ".log",
	}

	return config
}

func setLevel(level zapcore.Level) *zap.SugaredLogger {
	config.Level = zap.NewAtomicLevelAt(level)
	logger, err := config.Build()

	if err != nil {
		log.Fatalf("Failed to build logger: %v", err)
	}

	Log = logger.Sugar()

	return Log
}

func CreateLogger(debug *bool) *zap.SugaredLogger {
    if Log == nil {
        lock.Lock()
        defer lock.Unlock()

        if Log == nil {
			if *debug {
				return setLevel(zap.DebugLevel)
			} else {
				return setLevel(zap.ErrorLevel)
			}
        }
    }

    return Log
}

func SetLevelFromEnv(level string) *zap.SugaredLogger {
	switch level {
	case "debug":
		return setLevel(zap.DebugLevel)
	case "verbose":
		return setLevel(zap.InfoLevel)
	case "info":
		return setLevel(zap.InfoLevel)
	case "warn":
		return setLevel(zap.WarnLevel)
	case "error":
		return setLevel(zap.ErrorLevel)
	default:
		Log.Warnw("Invalid LOG_LEVEL in environment variables. Defaulting to INFO.", "provided_level", level)
		return setLevel(zap.InfoLevel)
	}
}