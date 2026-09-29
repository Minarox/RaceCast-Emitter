//! System telemetry: temperatures, CPU/GPU load, memory, NVENC clock, disk space, recording state and
//! power mode. Everything is read from `/proc` and `/sys` without root.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use super::{CsvRecorder, Output, Value};
use crate::capture::{ActiveDevice, DeviceKind};
use crate::storage;

const GPU_LOAD: &str = "/sys/devices/platform/bus@0/17000000.gpu/load";
const NVENC_FREQ: &str = "/sys/class/devfreq/154c0000.nvenc/cur_freq";
const NVPMODEL_STATUS: &str = "/var/lib/nvpmodel/status";
const NVPMODEL_CONF: &str = "/etc/nvpmodel.conf";

pub const COLUMNS: &[&str] = &[
    "cpu_temp_c",
    "gpu_temp_c",
    "tj_temp_c",
    "cpu_load_pct",
    "gpu_load_pct",
    "ram_used_mb",
    "nvenc_mhz",
    "disk_free_gb",
    "recording",
    "cameras",
    "mics",
    "livekit_connected",
    "power_mode",
];

/// Aggregated CPU times from the first line of `/proc/stat`.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct CpuTimes {
    idle: u64,
    total: u64,
}

pub fn parse_proc_stat(text: &str) -> Option<CpuTimes> {
    let line = text.lines().find(|l| l.starts_with("cpu "))?;
    let v: Vec<u64> = line.split_whitespace().skip(1).filter_map(|x| x.parse().ok()).collect();
    // user nice system idle iowait irq softirq steal
    let idle = v.get(3)? + v.get(4).copied().unwrap_or(0);
    Some(CpuTimes { idle, total: v.iter().take(8).sum() })
}

/// CPU load (%) between two samples.
pub fn cpu_load(prev: CpuTimes, cur: CpuTimes) -> Option<f64> {
    let total = cur.total.checked_sub(prev.total)?;
    let idle = cur.idle.checked_sub(prev.idle)?;
    (total > 0).then(|| (total - idle.min(total)) as f64 * 100.0 / total as f64)
}

/// Used memory (MB) = MemTotal − MemAvailable.
pub fn parse_meminfo(text: &str) -> Option<u64> {
    let field = |name: &str| text.lines().find(|l| l.starts_with(name))?.split_whitespace().nth(1)?.parse::<u64>().ok();
    Some(field("MemTotal:")?.saturating_sub(field("MemAvailable:")?) / 1024)
}

/// Power mode name (e.g. `15W`) from the nvpmodel status (`pmode:0002`) and configuration.
pub fn power_mode_name(status: &str, conf: &str) -> Option<String> {
    let id: u32 = status.trim().strip_prefix("pmode:")?.parse().ok()?;
    let tag = format!("< POWER_MODEL ID={id} NAME=");
    let line = conf.lines().find(|l| l.trim_start().starts_with(&tag))?;
    let name = line.trim_start().strip_prefix(&tag)?.trim_end().trim_end_matches('>').trim();
    Some(name.to_string())
}

/// Thermal zone type → temperature file.
fn thermal_zones() -> HashMap<String, PathBuf> {
    let Ok(dir) = std::fs::read_dir("/sys/class/thermal") else { return HashMap::new() };
    dir.flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with("thermal_zone"))
        .filter_map(|e| {
            let kind = std::fs::read_to_string(e.path().join("type")).ok()?;
            Some((kind.trim().to_string(), e.path().join("temp")))
        })
        .collect()
}

fn read_number(path: &Path) -> Option<f64> {
    std::fs::read_to_string(path).ok()?.trim().parse().ok()
}

pub struct Sources {
    pub devices: watch::Receiver<Vec<ActiveDevice>>,
    /// LiveKit connection state; `None` when streaming is disabled (empty field).
    pub livekit: Option<watch::Receiver<bool>>,
}

/// Samples the system state every `period` until `token` is cancelled.
pub async fn run(period: Duration, out: Output, src: Sources, token: CancellationToken) -> Result<(), String> {
    let (recordings_dir, recording) = (out.dir.clone(), out.gate.clone());
    let mut csv = CsvRecorder::new("system", COLUMNS, out);
    let zones = thermal_zones();
    let temp = |kind: &str| zones.get(kind).and_then(|p| read_number(p)).map(|m| m / 1000.0);
    let mut prev_cpu = std::fs::read_to_string("/proc/stat").ok().and_then(|t| parse_proc_stat(&t));
    let mut tick = tokio::time::interval(period);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    tick.tick().await; // first CPU load sample needs a previous one
    loop {
        tokio::select! {
            _ = tick.tick() => {}
            () = token.cancelled() => return Ok(()),
        }
        let cpu = std::fs::read_to_string("/proc/stat").ok().and_then(|t| parse_proc_stat(&t));
        let load = prev_cpu.zip(cpu).and_then(|(p, c)| cpu_load(p, c));
        prev_cpu = cpu.or(prev_cpu);
        let ram = std::fs::read_to_string("/proc/meminfo").ok().and_then(|t| parse_meminfo(&t));
        let disk = storage::free_bytes(&recordings_dir).ok().map(|b| b as f64 / 1e9);
        let power_mode = std::fs::read_to_string(NVPMODEL_STATUS)
            .ok()
            .zip(std::fs::read_to_string(NVPMODEL_CONF).ok())
            .and_then(|(s, c)| power_mode_name(&s, &c));
        let (cameras, mics) = {
            let devices = src.devices.borrow();
            let count = |k: DeviceKind| devices.iter().filter(|d| d.kind == k).count() as i64;
            (count(DeviceKind::Camera), count(DeviceKind::Microphone))
        };
        csv.record(&[
            Value::float(temp("cpu-thermal"), 1),
            Value::float(temp("gpu-thermal"), 1),
            Value::float(temp("tj-thermal"), 1),
            Value::float(load, 1),
            Value::float(read_number(Path::new(GPU_LOAD)).map(|l| l / 10.0), 1),
            Value::int(ram.and_then(|mb| i64::try_from(mb).ok())),
            Value::int(read_number(Path::new(NVENC_FREQ)).map(|hz| (hz / 1e6).round() as i64)),
            Value::float(disk, 1),
            Value::Bool(Some(*recording.borrow())),
            Value::int(Some(cameras)),
            Value::int(Some(mics)),
            Value::Bool(src.livekit.as_ref().map(|c| *c.borrow())),
            Value::text(power_mode),
        ]);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cpu_load_between_samples() {
        let a = parse_proc_stat("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4").unwrap();
        let b = parse_proc_stat("cpu  150 0 150 800 100 0 0 0 0 0").unwrap();
        assert_eq!(a, CpuTimes { idle: 800, total: 1000 });
        assert_eq!(cpu_load(a, b), Some(50.0));
        assert_eq!(cpu_load(b, a), None);
    }

    #[test]
    fn meminfo() {
        let t = "MemTotal:       15655436 kB\nMemFree:  100 kB\nMemAvailable:   11546632 kB\n";
        assert_eq!(parse_meminfo(t), Some((15_655_436 - 11_546_632) / 1024));
        assert_eq!(parse_meminfo("MemTotal: 1 kB"), None);
    }

    #[test]
    fn nvpmodel() {
        let conf = "< POWER_MODEL ID=0 NAME=MAXN >\nCPU_ONLINE CORE_0 1\n< POWER_MODEL ID=2 NAME=15W >\n";
        assert_eq!(power_mode_name("pmode:0002", conf).as_deref(), Some("15W"));
        assert_eq!(power_mode_name("pmode:0000\n", conf).as_deref(), Some("MAXN"));
        assert_eq!(power_mode_name("pmode:0007", conf), None);
    }
}
