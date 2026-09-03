package ups

import (
	"math"
	"os"
	"strconv"
	"sync"

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

// read returns the raw register value and whether the read succeeded. A
// failed read (I2C bus glitch — plausible in a vibrating vehicle) must never
// be silently reported as a valid 0 reading: the caller uses ok to decide
// whether to trust the resulting Data.
func read(reg byte) (uint16, bool) {
	mu.Lock()
	defer mu.Unlock()

	if device == nil {
		return 0, false
	}
	val, err := device.ReadRegU16BE(reg)
	if err != nil {
		logger.Error("[ups] Read register 0x%02X: %v", reg, err)
		return 0, false
	}
	return val, true
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

// Read returns the current UPS measurements. ok is false if any register
// read failed — callers must not treat the zero-valued Data as a real
// reading in that case (e.g. it must not be reported as "battery empty").
func Read() (Data, bool) {
	vRaw, ok1 := read(regVoltage)
	aRaw, ok2 := read(regCurrent)
	wRaw, ok3 := read(regPower)
	if !ok1 || !ok2 || !ok3 {
		return Data{}, false
	}
	return decode(vRaw, aRaw, wRaw), true
}

// decode converts the three raw INA219 register values (BE16, as read())
// into engineering units — split out from Read() so this arithmetic (the
// >>3 bus-voltage shift, current/power's int16 sign handling, the
// percentage clamp) is testable without real I2C hardware.
func decode(vRaw, aRaw, wRaw uint16) Data {
	v := float64(vRaw>>3) * 0.004
	a := float64(int16(aRaw)) * currentLSB / 1000
	w := float64(int16(wRaw)) * powerLSB

	p := (v - batteryMin) / (batteryMax - batteryMin) * 100
	p = math.Max(0, math.Min(p, 100))

	return Data{
		Voltage:    round2(v),
		Current:    round2(a),
		Power:      round2(w),
		Percentage: round2(p),
	}
}
