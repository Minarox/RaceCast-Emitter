//! UPS telemetry: Waveshare UPS Power Module C, INA219 on I2C. Same calibration and conversions as the
//! reference script (`UPS_Power_Module_C/ina219.py`). No shutdown threshold: the UPS is only a telemetry
//! source (SPEC §5b).

use std::time::Duration;

use i2cdev::core::I2CDevice;
use i2cdev::linux::{LinuxI2CDevice, LinuxI2CError};
use tokio_util::sync::CancellationToken;

use super::{CsvRecorder, Output, Value};

const REG_CONFIG: u8 = 0x00;
const REG_BUS_VOLTAGE: u8 = 0x02;
const REG_POWER: u8 = 0x03;
const REG_CURRENT: u8 = 0x04;
const REG_CALIBRATION: u8 = 0x05;

const CALIBRATION: u16 = 26868;
/// 16 V range, gain /2 (80 mV), 12-bit ADC with 32 samples for bus and shunt, continuous mode.
const CONFIG: u16 = (0x01 << 11) | (0x0D << 7) | (0x0D << 3) | 0x07; // bit 13 (bus range) = 0: 16 V
const CURRENT_LSB_MA: f64 = 0.1524;
const POWER_LSB_W: f64 = 0.003048;
/// 3S pack: 9 V = empty, 12.6 V = full.
const EMPTY_V: f64 = 9.0;
const RANGE_V: f64 = 3.6;

pub const COLUMNS: &[&str] = &["load_voltage_v", "current_a", "power_w", "percent"];

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Reading {
    pub load_voltage_v: f64,
    pub current_a: f64,
    pub power_w: f64,
    pub percent: f64,
}

/// Register values → physical values (registers are big-endian 16-bit words).
pub fn convert(bus_raw: u16, current_raw: u16, power_raw: u16) -> Reading {
    let load_voltage_v = f64::from(bus_raw >> 3) * 0.004;
    Reading {
        load_voltage_v,
        current_a: f64::from(current_raw as i16) * CURRENT_LSB_MA / 1000.0,
        power_w: f64::from(power_raw as i16) * POWER_LSB_W,
        percent: ((load_voltage_v - EMPTY_V) / RANGE_V * 100.0).clamp(0.0, 100.0),
    }
}

struct Ina219 {
    dev: LinuxI2CDevice,
}

impl Ina219 {
    fn open(bus: u8, address: u8) -> Result<Self, LinuxI2CError> {
        let mut ina = Self { dev: LinuxI2CDevice::new(format!("/dev/i2c-{bus}"), u16::from(address))? };
        ina.configure()?;
        Ok(ina)
    }

    fn read_reg(&mut self, reg: u8) -> Result<u16, LinuxI2CError> {
        let d = self.dev.smbus_read_i2c_block_data(reg, 2)?;
        match d.as_slice() {
            [hi, lo] => Ok(u16::from_be_bytes([*hi, *lo])),
            _ => Err(LinuxI2CError::Io(std::io::Error::other("short I2C read"))),
        }
    }

    fn write_reg(&mut self, reg: u8, value: u16) -> Result<(), LinuxI2CError> {
        self.dev.smbus_write_i2c_block_data(reg, &value.to_be_bytes())
    }

    fn configure(&mut self) -> Result<(), LinuxI2CError> {
        self.write_reg(REG_CALIBRATION, CALIBRATION)?;
        self.write_reg(REG_CONFIG, CONFIG)
    }

    fn read(&mut self) -> Result<Reading, LinuxI2CError> {
        // The INA219 reverts to its power-on defaults if it loses power: reapply the configuration.
        if self.read_reg(REG_CONFIG)? != CONFIG || self.read_reg(REG_CALIBRATION)? != CALIBRATION {
            tracing::warn!("INA219 configuration lost, reapplying it");
            self.configure()?;
        }
        Ok(convert(self.read_reg(REG_BUS_VOLTAGE)?, self.read_reg(REG_CURRENT)?, self.read_reg(REG_POWER)?))
    }
}

/// Samples the UPS every `period` until `token` is cancelled. I2C errors leave empty fields and reopen the
/// device on the next sample.
pub async fn run(bus: u8, address: u8, period: Duration, out: Output, token: CancellationToken) -> Result<(), String> {
    let mut csv = CsvRecorder::new("ups", COLUMNS, out);
    let mut ina: Option<Ina219> = None;
    let mut failing = false;
    let mut tick = tokio::time::interval(period);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        tokio::select! {
            _ = tick.tick() => {}
            () = token.cancelled() => return Ok(()),
        }
        let result = match ina.as_mut() {
            Some(dev) => dev.read(),
            None => Ina219::open(bus, address).and_then(|mut dev| {
                let r = dev.read();
                ina = Some(dev);
                r
            }),
        };
        match result {
            Ok(r) => {
                if std::mem::take(&mut failing) {
                    tracing::info!(voltage_v = r.load_voltage_v, percent = r.percent, "UPS readings back");
                }
                csv.record(&[
                    Value::float(Some(r.load_voltage_v), 3),
                    Value::float(Some(r.current_a), 4),
                    Value::float(Some(r.power_w), 3),
                    Value::float(Some(r.percent), 1),
                ]);
            }
            Err(e) => {
                if !std::mem::replace(&mut failing, true) {
                    tracing::warn!(bus, address = %format!("{address:#04x}"), error = %e, "UPS unreadable");
                }
                ina = None;
                csv.record(&[
                    Value::float(None, 3),
                    Value::float(None, 4),
                    Value::float(None, 3),
                    Value::float(None, 1),
                ]);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_matches_the_reference_script() {
        assert_eq!(CONFIG, 0x0EEF);
    }

    #[test]
    fn conversions() {
        // Bus register read on the machine (0x619A → 12.49 V).
        let r = convert(0x619A, 0, 0);
        assert!((r.load_voltage_v - 12.492).abs() < 1e-9, "{r:?}");
        assert!((r.percent - 97.0).abs() < 0.01, "{r:?}");
        // Signed current and power.
        let r = convert(0x619A, (-1000i16) as u16, 500);
        assert!((r.current_a + 0.1524).abs() < 1e-9, "{r:?}");
        assert!((r.power_w - 1.524).abs() < 1e-9, "{r:?}");
        // Percentage clamped.
        assert_eq!(convert(8 << 3, 0, 0).percent, 0.0);
        assert_eq!(convert(4000 << 3, 0, 0).percent, 100.0);
    }
}
