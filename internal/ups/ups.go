package ups

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	i2c "github.com/d2r2/go-i2c"
	golog "github.com/d2r2/go-logger"

	"racecast-emitter/internal/logger"
)

// https://www.waveshare.com/wiki/UPS_Power_Module_(C)

const (
	regConfig       = 0x00
	regShuntVoltage = 0x01
	regVoltage      = 0x02
	regPower        = 0x03
	regCurrent      = 0x04
	regCalibration  = 0x05

	range16V       = 0x00
	div2_80mV      = 0x01
	adcRes12b32s   = 0x0d
	modeContinuous = 0x07

	currentLSB = 0.1524
	calValue   = 26868
	powerLSB   = 0.003048

	// Batterie 3S LiPo : 9V vide, 12.6V pleine
	batteryMin = 9.0
	batteryMax = 12.6
)

// Defaults I2C (Waveshare UPS Power Module C sur Jetson Orin NX)
const (
	defaultAddr = uint8(0x40)
	defaultBus  = 7
)

var (
	mu     sync.Mutex
	device *i2c.I2C
)

// envUint8 reads an environment variable as hex or decimal.
func envUint8(key string, def uint8) uint8 {
	if s := os.Getenv(key); s != "" {
		v, err := strconv.ParseUint(s, 0, 8)
		if err == nil {
			return uint8(v)
		}
		logger.Warn("[ups] Invalid variable %s, using default: 0x%02X", key, def)
	}
	return def
}

func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		v, err := strconv.Atoi(s)
		if err == nil && v >= 0 {
			return v
		}
		logger.Warn("[ups] Invalid variable %s, using default: %d", key, def)
	}
	return def
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// Open opens the I2C connection to the UPS and calibrates it.
// Address and bus are read from RC_UPS_ADDR and RC_UPS_BUS (defaults: 0x40, 7).
func Open() error {
	mu.Lock()
	defer mu.Unlock()

	if device != nil {
		return nil
	}

	addr := envUint8("RC_UPS_ADDR", defaultAddr)
	bus := envInt("RC_UPS_BUS", defaultBus)

	conn, err := i2c.NewI2C(addr, bus)
	if err != nil {
		return err
	}

	// Silence debug logs from the go-i2c library.
	_ = golog.ChangePackageLogLevel("i2c", golog.PanicLevel)

	device = conn
	logger.Info("[ups] Connected (address 0x%02X, bus %d)", addr, bus)
	calibrate()
	return nil
}

// Close closes the I2C connection.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if device != nil {
		device.Close()
		device = nil
	}
}

func read(reg byte) uint16 {
	mu.Lock()
	defer mu.Unlock()

	if device == nil {
		return 0
	}
	val, err := device.ReadRegU16BE(reg)
	if err != nil {
		logger.Error("[ups] Read register 0x%02X: %v", reg, err)
		return 0
	}
	return val
}

func write(reg byte, value uint16) {
	mu.Lock()
	defer mu.Unlock()

	if device == nil {
		return
	}
	if err := device.WriteRegU16BE(reg, value); err != nil {
		logger.Error("[ups] Write register 0x%02X: %v", reg, err)
	}
}

func calibrate() {
	// Internal call — mu already held, write directly.
	if err := device.WriteRegU16BE(regCalibration, calValue); err != nil {
		logger.Error("[ups] Calibration: %v", err)
		return
	}

	cfg := (range16V << 13) |
		(div2_80mV << 11) |
		(adcRes12b32s << 7) |
		(adcRes12b32s << 3) |
		modeContinuous

	if err := device.WriteRegU16BE(regConfig, uint16(cfg)); err != nil {
		logger.Error("[ups] Configuration: %v", err)
	}
}

// Data holds measurements read from the UPS.
type Data struct {
	Voltage    float64 // V
	Current    float64 // A
	Power      float64 // W
	Percentage float64 // 0–100 %
}

// Read lit les valeurs courantes de l'UPS.
func Read() Data {
	v := float64(read(regVoltage)>>3) * 0.004
	a := float64(int16(read(regCurrent))) * currentLSB / 1000
	w := float64(int16(read(regPower))) * powerLSB

	p := (v - batteryMin) / (batteryMax - batteryMin) * 100
	p = math.Max(0, math.Min(p, 100))

	return Data{
		Voltage:    round2(v),
		Current:    round2(a),
		Power:      round2(w),
		Percentage: round2(p),
	}
}

// Run reads and logs UPS values at the given interval until ctx is cancelled.
func Run(ctx context.Context, interval time.Duration) {
	if err := Open(); err != nil {
		logger.Fatal("[ups] Failed to open I2C connection: %v", err)
	}
	defer Close()

	logger.Info("[ups] Reading every %s (Ctrl+C to stop)", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	first := true
	for {
		if !first {
			// Move up one line and erase it to overwrite the previous value.
			fmt.Fprint(os.Stdout, "\033[1A\033[2K")
		}
		d := Read()
		logger.Info("[ups] %.2f V  %.2f A  %.2f W  %.1f %%",
			d.Voltage, d.Current, d.Power, d.Percentage)
		first = false

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
