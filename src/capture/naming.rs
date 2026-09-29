//! Automatic device names, used when `devices.yml` gives none.

use super::DeviceKind;
use crate::config::limits::NAME_MAX_LEN;

/// `cam-<model>` / `mic-<model>`, e.g. `HD_USB_Camera` → `cam-hd-usb-camera`.
pub fn auto_name(kind: DeviceKind, model: &str) -> String {
    let prefix = match kind {
        DeviceKind::Camera => "cam",
        DeviceKind::Microphone => "mic",
    };
    let mut slug = String::new();
    for c in model.chars() {
        if c.is_ascii_alphanumeric() {
            slug.push(c.to_ascii_lowercase());
        } else if !slug.is_empty() && !slug.ends_with('-') {
            slug.push('-');
        }
    }
    let slug = slug.trim_end_matches('-');
    let slug = if slug.is_empty() { "device" } else { slug };
    truncate(&format!("{prefix}-{slug}"), NAME_MAX_LEN)
}

/// `name` if free, otherwise `name-2`, `name-3`… (still within the length limit).
pub fn unique(name: &str, taken: impl Fn(&str) -> bool) -> String {
    if !taken(name) {
        return name.to_string();
    }
    (2..)
        .map(|n| {
            let suffix = format!("-{n}");
            format!("{}{suffix}", truncate(name, NAME_MAX_LEN - suffix.len()))
        })
        .find(|candidate| !taken(candidate))
        .unwrap_or_else(|| name.to_string())
}

fn truncate(name: &str, max: usize) -> String {
    name.chars().take(max).collect::<String>().trim_end_matches('-').to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::validate_name;

    #[test]
    fn names_from_models() {
        assert_eq!(auto_name(DeviceKind::Camera, "HD_USB_Camera"), "cam-hd-usb-camera");
        assert_eq!(auto_name(DeviceKind::Microphone, "USB_Audio_Device"), "mic-usb-audio-device");
        assert_eq!(auto_name(DeviceKind::Camera, "__"), "cam-device");
        let long = auto_name(DeviceKind::Camera, "A_Very_Long_Model_Name_From_Some_Vendor");
        assert!(long.len() <= NAME_MAX_LEN, "{long}");
        for n in [long, auto_name(DeviceKind::Microphone, "Ünïcode Mic 2")] {
            assert!(validate_name(&n).is_ok(), "{n}");
        }
    }

    #[test]
    fn unique_suffixes() {
        let taken = ["cam-a", "cam-a-2"];
        assert_eq!(unique("cam-b", |n| taken.contains(&n)), "cam-b");
        assert_eq!(unique("cam-a", |n| taken.contains(&n)), "cam-a-3");
        let long = "c".repeat(NAME_MAX_LEN);
        let u = unique(&long, |n| n == long);
        assert!(u.len() <= NAME_MAX_LEN && u.ends_with("-2"), "{u}");
    }
}
