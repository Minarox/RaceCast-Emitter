//! Device discovery through udev: enumeration of the USB cameras/microphones present, and a monitor that
//! reports hot-plug events. udev events arrive once the `/dev/…/by-id` and `by-path` links exist.

use std::ffi::OsStr;
use std::path::Path;
use std::time::Duration;

use rustix::event::{PollFd, PollFlags, Timespec};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use super::{DeviceEvent, DeviceInfo, DeviceKind};

const SUBSYSTEMS: [&str; 2] = ["video4linux", "sound"];
const POLL_TIMEOUT: Duration = Duration::from_millis(500);

/// Lists the USB capture devices currently present.
pub fn enumerate() -> std::io::Result<Vec<DeviceInfo>> {
    let mut found = Vec::new();
    for subsystem in SUBSYSTEMS {
        let mut e = udev::Enumerator::new()?;
        e.match_subsystem(subsystem)?;
        found.extend(e.scan_devices()?.filter_map(|d| device_info(&d)));
    }
    Ok(found)
}

/// Watches udev until `token` is cancelled and forwards the changes to `tx`. Blocking: run it on a
/// dedicated thread (`spawn_blocking`).
pub fn watch(tx: mpsc::Sender<DeviceEvent>, token: CancellationToken) -> std::io::Result<()> {
    let mut builder = udev::MonitorBuilder::new()?;
    for subsystem in SUBSYSTEMS {
        builder = builder.match_subsystem(subsystem)?;
    }
    let socket = builder.listen()?;
    // Listening started: ask the manager to re-enumerate, so nothing that happened before is missed.
    let _ = tx.blocking_send(DeviceEvent::Resync);
    let timeout = Timespec { tv_sec: 0, tv_nsec: POLL_TIMEOUT.subsec_nanos().into() };
    while !token.is_cancelled() {
        let mut fds = [PollFd::new(&socket, PollFlags::IN)];
        match rustix::event::poll(&mut fds, Some(&timeout)) {
            Ok(_) => {}
            Err(rustix::io::Errno::INTR) => continue,
            Err(e) => return Err(e.into()),
        }
        for event in socket.iter() {
            let forwarded = match event.event_type() {
                udev::EventType::Add => device_info(&event).map(DeviceEvent::Added),
                udev::EventType::Remove => {
                    Some(DeviceEvent::Removed { syspath: event.syspath().to_string_lossy().into_owned() })
                }
                _ => None,
            };
            if let Some(ev) = forwarded
                && tx.blocking_send(ev).is_err()
            {
                return Ok(()); // manager gone: shutting down
            }
        }
    }
    Ok(())
}

/// Keeps USB capture devices only: V4L2 capture nodes (not the metadata nodes) and ALSA cards.
fn device_info(d: &udev::Device) -> Option<DeviceInfo> {
    if prop(d, "ID_BUS").as_deref() != Some("usb") {
        return None;
    }
    let devnode = d.devnode()?.to_string_lossy().into_owned();
    let subsystem = d.subsystem().and_then(OsStr::to_str)?;
    let (kind, alsa_card) = match subsystem {
        "video4linux" if prop(d, "ID_V4L_CAPABILITIES").is_some_and(|c| c.contains(":capture:")) => {
            (DeviceKind::Camera, None)
        }
        // One control device per ALSA card; the card's `id` names it for `hw:CARD=<id>`.
        "sound" if d.sysname().to_string_lossy().starts_with("controlC") => {
            let card = d.parent().and_then(|p| p.attribute_value("id").map(|v| v.to_string_lossy().into_owned()));
            (DeviceKind::Microphone, Some(card?))
        }
        _ => return None,
    };
    let links = prop(d, "DEVLINKS").unwrap_or_default();
    let by_id_dir = if kind == DeviceKind::Camera { "/dev/v4l/by-id/" } else { "/dev/snd/by-id/" };
    let by_id = links
        .split_whitespace()
        .find(|l| l.starts_with(by_id_dir))
        .and_then(|l| Path::new(l).file_name())
        .map(|n| n.to_string_lossy().into_owned());
    Some(DeviceInfo {
        kind,
        syspath: d.syspath().to_string_lossy().into_owned(),
        devnode,
        usb_port: prop(d, "ID_PATH").as_deref().map(usb_port),
        by_id,
        model: prop(d, "ID_MODEL").unwrap_or_else(|| "device".into()),
        alsa_card,
    })
}

fn prop(d: &udev::Device, key: &str) -> Option<String> {
    d.property_value(key).map(|v| v.to_string_lossy().into_owned())
}

/// Physical port from `ID_PATH`: drops the trailing `:<config>.<interface>`, so that the video and audio
/// interfaces of the same camera share the same port.
fn usb_port(id_path: &str) -> String {
    match id_path.rsplit_once(':') {
        Some((port, iface)) if iface.contains('.') && iface.chars().all(|c| c.is_ascii_digit() || c == '.') => {
            port.to_string()
        }
        _ => id_path.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn usb_port_drops_the_interface() {
        assert_eq!(usb_port("platform-3610000.usb-usb-0:2.1:1.0"), "platform-3610000.usb-usb-0:2.1");
        assert_eq!(usb_port("platform-3610000.usb-usb-0:2.1:1.2"), "platform-3610000.usb-usb-0:2.1");
        assert_eq!(usb_port("platform-3610000.usb-usb-0:2.4.3:1.0"), "platform-3610000.usb-usb-0:2.4.3");
        assert_eq!(usb_port("platform-sound"), "platform-sound");
    }
}
