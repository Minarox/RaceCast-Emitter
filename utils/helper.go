package utils

import (
	"math"
	"strconv"
)

func RoundToTwoDecimals(n float64) float64 {
    return math.Round(n*100) / 100
}

func ParseFloat32(s string) *float32 {
	if s == "" {
		return nil
	}
	if val, err := strconv.ParseFloat(s, 32); err == nil {
		v := float32(val)
		return &v
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