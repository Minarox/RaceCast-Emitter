package utils

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func BoolPtr(b bool) *bool {
	return &b
}

func RoundToTwoDecimals(n float64) float64 {
    return math.Round(n*100) / 100
}

func ParseFloat32(s string) *float32 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	// Accept comma as decimal separator (e.g., French locales)
	s = strings.ReplaceAll(s, ",", ".")

	// Try direct parse first
	if val, err := strconv.ParseFloat(s, 32); err == nil {
		v := float32(val)
		return &v
	}

	// Fallback: extract first floating-point substring (handles units or extra text)
	re := regexp.MustCompile(`[-+]?\d*\.?\d+(?:[eE][-+]?\d+)?`)
	match := re.FindString(s)
	if match != "" {
		if val, err := strconv.ParseFloat(match, 32); err == nil {
			v := float32(val)
			return &v
		}
	}

	return nil
}

func ParseInt(s string) *int {
	if s == "" {
		return nil
	}
	if val, err := strconv.Atoi(s); err == nil {
		return &val
	}
	return nil
}

// parseIntEnv reads an environment variable as an integer, returning def when
// the variable is absent or cannot be parsed.
func ParseIntEnv(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		Log.Warnw("Invalid integer in environment variable; using default.",
			"key", key, "value", s, "default", def)
		return def
	}
	return v
}