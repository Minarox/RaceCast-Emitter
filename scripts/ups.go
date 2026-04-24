package scripts

import (
	"math"
	"racecast-emitter/utils"
	"sync"

	i2c "github.com/d2r2/go-i2c"
	"github.com/d2r2/go-logger"
)

// https://www.waveshare.com/wiki/UPS_Power_Module_(C)

const (
	REG_CONFIG       = 0x00
	REG_SHUNTVOLTAGE = 0x01
	REG_VOLTAGE      = 0x02
	REG_POWER        = 0x03
	REG_CURRENT      = 0x04
	REG_CALIBRATION  = 0x05

	RANGE_16V        = 0x00
	DIV_2_80MV       = 0x01
	ADCRES_12BIT_32S = 0x0d
	MODE_CONTINUOUS  = 0x07

	CURRENT_LSB      = 0.1524
	CAL_VALUE        = 26868
	POWER_LSB        = 0.003048
)

var (
	lock = &sync.Mutex{}
	ups *i2c.I2C
)

func read(register byte) uint16 {
	val, err := ups.ReadRegU16BE(register)
	if err != nil {
		utils.Log.Errorw("Failed to read from UPS.", "register", register, "error", err)
		return 0
	}

	return val
}

func write(register byte, value uint16) {
	err := ups.WriteRegU16BE(register, value)
	if err != nil {
		utils.Log.Errorw("Failed to write to UPS.", "register", register, "error", err)
	}
}

func setCalibration16V5A() {
	write(REG_CALIBRATION, CAL_VALUE)

	config := (RANGE_16V << 13) |
		(DIV_2_80MV << 11) |
		(ADCRES_12BIT_32S << 7) |
		(ADCRES_12BIT_32S << 3) |
		MODE_CONTINUOUS

	write(REG_CONFIG, uint16(config))
}

func getBusVoltage_V() float64 {
	val := read(REG_VOLTAGE)
	return float64(val>>3) * 0.004
}

func getCurrent_A() float64 {
	val := int16(read(REG_CURRENT))
	return float64(val) * CURRENT_LSB / 1000
}

func getPower_W() float64 {
	val := int16(read(REG_POWER))
	return float64(val) * POWER_LSB
}

func CreateUPSReader(addr uint8, bus int) *i2c.I2C {
	if ups == nil {
		lock.Lock()
        defer lock.Unlock()

		if ups == nil {
			connection, err := i2c.NewI2C(addr, bus)
			if err != nil {
				utils.Log.Fatalw("Failed to create I2C client for UPS.", "details", err)
			}

			ups = connection
			logger.ChangePackageLogLevel("i2c", logger.LogLevel(utils.Log.Level()))
			utils.Log.Infow("Successfully created I2C client for UPS.", "address", addr, "bus", bus)
			setCalibration16V5A()
		}
	}

	return ups
}

func GetUPSData() map[string]any {
	if ups == nil {
		utils.Log.Warn("UPS reader not initialized.")
		return nil
	}
	
	v := getBusVoltage_V()
	a := getCurrent_A()
	w := getPower_W()

	p := ((v - 9) / 3.6) * 100
	p = math.Max(0, math.Min(p, 100))

	var data = map[string]any{
		"v": utils.RoundToThreeDecimals(v),
		"a": utils.RoundToThreeDecimals(a),
		"w": utils.RoundToThreeDecimals(w),
		"p": utils.RoundToThreeDecimals(p),
	}

	utils.Log.Infow("UPS Data", "payload", data)
	return data
}

func CloseUPSReader() {
	if ups != nil {
		lock.Lock()
		defer lock.Unlock()

		ups.Close()
		ups = nil
	}
}