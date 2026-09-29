//! Configuration: `.env` (connection, paths, defaults) and `devices.yml` (per-device overrides).

mod devices;
mod effective;
mod paths;
mod settings;
mod types;

pub use devices::{DevicesFile, DevicesStore};
pub use effective::{CameraConfig, MicConfig};
pub use settings::{LiveKitSettings, Settings};
#[cfg(test)]
pub use types::validate_name;
pub use types::{Resolution, limits};
