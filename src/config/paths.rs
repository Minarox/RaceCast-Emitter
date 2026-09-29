//! Resolution of configuration paths.

use std::path::{Component, Path, PathBuf};

/// Resolves `value`: `~` → `home`, relative path → relative to `base` (the `.env` directory, not the
/// current directory, which depends on how the program is launched). `.` components are removed.
pub fn resolve(base: &Path, home: Option<&Path>, value: &str) -> PathBuf {
    let expanded = match (value.strip_prefix('~'), home) {
        (Some(""), Some(home)) => home.to_path_buf(),
        (Some(rest), Some(home)) if rest.starts_with('/') => home.join(rest.trim_start_matches('/')),
        _ => PathBuf::from(value),
    };
    let absolute = if expanded.is_absolute() { expanded } else { base.join(expanded) };
    absolute.components().filter(|c| *c != Component::CurDir).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    const BASE: &str = "/opt/racecast";
    const HOME: &str = "/home/jetson";

    fn r(value: &str) -> PathBuf {
        resolve(Path::new(BASE), Some(Path::new(HOME)), value)
    }

    #[test]
    fn relative_to_env_dir() {
        assert_eq!(r("./records"), PathBuf::from("/opt/racecast/records"));
        assert_eq!(r("logs"), PathBuf::from("/opt/racecast/logs"));
        assert_eq!(r("./conf/./devices.yml"), PathBuf::from("/opt/racecast/conf/devices.yml"));
    }

    #[test]
    fn absolute_kept() {
        assert_eq!(r("/var/lib/racecast"), PathBuf::from("/var/lib/racecast"));
    }

    #[test]
    fn tilde_expanded() {
        assert_eq!(r("~"), PathBuf::from("/home/jetson"));
        assert_eq!(r("~/records"), PathBuf::from("/home/jetson/records"));
        // `~user` is not expanded: treated as a relative name.
        assert_eq!(r("~other/x"), PathBuf::from("/opt/racecast/~other/x"));
        assert_eq!(resolve(Path::new(BASE), None, "~/x"), PathBuf::from("/opt/racecast/~/x"));
    }
}
