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

// envUint8 lit une variable d'environnement en hexadécimal ou décimal.
func envUint8(key string, def uint8) uint8 {
	if s := os.Getenv(key); s != "" {
		v, err := strconv.ParseUint(s, 0, 8)
		if err == nil {
			return uint8(v)
		}
		logger.Warn("[ups] Variable %s invalide, valeur par défaut utilisée : 0x%02X", key, def)
	}
	return def
}

func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		v, err := strconv.Atoi(s)
		if err == nil && v >= 0 {
			return v
		}
		logger.Warn("[ups] Variable %s invalide, valeur par défaut utilisée : %d", key, def)
	}
	return def
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// Open ouvre la connexion I2C vers l'UPS et initialise la calibration.
// L'adresse et le bus sont lus depuis RC_UPS_ADDR et RC_UPS_BUS,
// avec respectivement 0x40 et 7 comme valeurs par défaut.
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

	// Silencer les logs de debug de la bibliothèque go-i2c
	_ = golog.ChangePackageLogLevel("i2c", golog.PanicLevel)

	device = conn
	logger.Info("[ups] Connecté (adresse 0x%02X, bus %d)", addr, bus)
	calibrate()
	return nil
}

// Close ferme la connexion I2C.
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
		logger.Error("[ups] Lecture registre 0x%02X : %v", reg, err)
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
		logger.Error("[ups] Écriture registre 0x%02X : %v", reg, err)
	}
}

func calibrate() {
	// Appel interne — mu déjà verrouillé, on écrit directement
	if err := device.WriteRegU16BE(regCalibration, calValue); err != nil {
		logger.Error("[ups] Calibration : %v", err)
		return
	}

	cfg := (range16V << 13) |
		(div2_80mV << 11) |
		(adcRes12b32s << 7) |
		(adcRes12b32s << 3) |
		modeContinuous

	if err := device.WriteRegU16BE(regConfig, uint16(cfg)); err != nil {
		logger.Error("[ups] Configuration : %v", err)
	}
}

// Data contient les mesures lues depuis l'UPS.
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

// Run lit et logue les valeurs de l'UPS à intervalle régulier jusqu'à
// ce que le contexte soit annulé.
func Run(ctx context.Context, interval time.Duration) {
	if err := Open(); err != nil {
		logger.Fatal("[ups] Impossible d'ouvrir la connexion I2C : %v", err)
	}
	defer Close()

	logger.Info("[ups] Lecture toutes les %s (Ctrl+C pour arrêter)", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	first := true
	for {
		if !first {
			// Remonte d'une ligne et l'efface pour écraser la valeur précédente
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
