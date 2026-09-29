//! Minimal ModemManager client over the system D-Bus. The program never talks to the modem's AT ports,
//! which ModemManager owns.

use std::collections::HashMap;

use zbus::zvariant::{OwnedObjectPath, OwnedValue, Value};

const SERVICE: &str = "org.freedesktop.ModemManager1";
const ROOT: &str = "/org/freedesktop/ModemManager1";
pub const MODEM: &str = "org.freedesktop.ModemManager1.Modem";
pub const MODEM_3GPP: &str = "org.freedesktop.ModemManager1.Modem.Modem3gpp";
pub const SIGNAL: &str = "org.freedesktop.ModemManager1.Modem.Signal";
pub const LOCATION: &str = "org.freedesktop.ModemManager1.Modem.Location";
const BEARER: &str = "org.freedesktop.ModemManager1.Bearer";

/// `MMModemPortType` of the NMEA port.
const PORT_TYPE_GPS: u32 = 5;
/// `MMModemLocationSource` bits.
pub const LOCATION_3GPP: u32 = 1 << 0;
const LOCATION_GPS_RAW: u32 = 1 << 1;
const LOCATION_GPS_NMEA: u32 = 1 << 2;
pub const LOCATION_GPS_UNMANAGED: u32 = 1 << 4;

pub type Properties = HashMap<String, OwnedValue>;

#[derive(Clone)]
pub struct ModemManager {
    conn: zbus::Connection,
}

/// Properties of the first modem, per interface.
pub struct Modem {
    pub path: OwnedObjectPath,
    interfaces: HashMap<String, Properties>,
}

impl ModemManager {
    pub async fn connect() -> zbus::Result<Self> {
        Ok(Self { conn: zbus::Connection::system().await? })
    }

    /// The first modem known to ModemManager, with all its properties (one D-Bus call).
    pub async fn modem(&self) -> zbus::Result<Option<Modem>> {
        let om = zbus::fdo::ObjectManagerProxy::builder(&self.conn)
            .destination(SERVICE)?
            .path(ROOT)?
            .cache_properties(zbus::proxy::CacheProperties::No)
            .build()
            .await?;
        let objects = om.get_managed_objects().await?;
        let mut modems: Vec<Modem> = objects
            .into_iter()
            .filter(|(_, ifaces)| ifaces.keys().any(|i| i.as_str() == MODEM))
            .map(|(path, ifaces)| Modem {
                path,
                interfaces: ifaces.into_iter().map(|(i, p)| (i.to_string(), p)).collect(),
            })
            .collect();
        modems.sort_by(|a, b| a.path.as_str().cmp(b.path.as_str()));
        Ok(modems.into_iter().next())
    }

    async fn proxy(&self, path: &str, interface: &'static str) -> zbus::Result<zbus::Proxy<'static>> {
        zbus::proxy::Builder::new(&self.conn)
            .destination(SERVICE)?
            .path(path.to_string())?
            .interface(interface)?
            .cache_properties(zbus::proxy::CacheProperties::No)
            .build()
            .await
    }

    pub async fn bearer_connected(&self, path: &str) -> zbus::Result<bool> {
        self.proxy(path, BEARER).await?.get_property::<bool>("Connected").await
    }

    /// Starts refreshing the extended signal values every `rate_s` seconds.
    pub async fn setup_signal(&self, modem: &str, rate_s: u32) -> zbus::Result<()> {
        self.proxy(modem, SIGNAL).await?.call_method("Setup", &(rate_s,)).await.map(|_| ())
    }

    /// 3GPP location string `MCC,MNC,LAC,CI,TAC`, if the source is enabled.
    pub async fn location_3gpp(&self, modem: &str) -> zbus::Result<Option<String>> {
        let loc: HashMap<u32, OwnedValue> = self.proxy(modem, LOCATION).await?.call("GetLocation", &()).await?;
        Ok(loc.get(&LOCATION_3GPP).and_then(|v| String::try_from(v.try_clone().ok()?).ok()))
    }

    /// Enables the location sources `wanted` on top of `enabled` (the GNSS modes are exclusive: RAW/NMEA
    /// are dropped when UNMANAGED is requested).
    pub async fn enable_location(&self, modem: &str, enabled: u32, wanted: u32) -> zbus::Result<()> {
        let mut sources = enabled | wanted;
        if wanted & LOCATION_GPS_UNMANAGED != 0 {
            sources &= !(LOCATION_GPS_RAW | LOCATION_GPS_NMEA);
        }
        self.proxy(modem, LOCATION).await?.call_method("Setup", &(sources, false)).await.map(|_| ())
    }

    pub async fn reset(&self, modem: &str) -> zbus::Result<()> {
        self.proxy(modem, MODEM).await?.call_method("Reset", &()).await.map(|_| ())
    }
}

impl Modem {
    fn prop(&self, interface: &str, name: &str) -> Option<Value<'_>> {
        self.interfaces.get(interface)?.get(name).and_then(|v| v.try_clone().ok()).map(Value::from)
    }

    pub fn i32(&self, interface: &str, name: &str) -> Option<i32> {
        i32::try_from(self.prop(interface, name)?).ok()
    }

    pub fn u32(&self, interface: &str, name: &str) -> Option<u32> {
        u32::try_from(self.prop(interface, name)?).ok()
    }

    pub fn string(&self, interface: &str, name: &str) -> Option<String> {
        String::try_from(self.prop(interface, name)?).ok().filter(|s| !s.is_empty())
    }

    /// `SignalQuality` = (percent, recent).
    pub fn signal_quality(&self) -> Option<u32> {
        <(u32, bool)>::try_from(self.prop(MODEM, "SignalQuality")?).ok().map(|(q, _)| q)
    }

    pub fn bearers(&self) -> Vec<String> {
        self.prop(MODEM, "Bearers")
            .and_then(|v| Vec::<OwnedObjectPath>::try_from(v).ok())
            .map(|v| v.into_iter().map(|p| p.to_string()).collect())
            .unwrap_or_default()
    }

    /// `/dev/…` path of the NMEA port.
    pub fn gps_port(&self) -> Option<String> {
        let ports = Vec::<(String, u32)>::try_from(self.prop(MODEM, "Ports")?).ok()?;
        ports.into_iter().find(|(_, kind)| *kind == PORT_TYPE_GPS).map(|(name, _)| format!("/dev/{name}"))
    }

    /// A dictionary property of the Signal interface (`Lte`, `Nr5g`): value name → number.
    pub fn signal(&self, name: &str) -> HashMap<String, f64> {
        self.prop(SIGNAL, name)
            .and_then(|v| HashMap::<String, OwnedValue>::try_from(v).ok())
            .map(|d| d.into_iter().filter_map(|(k, v)| Some((k, f64::try_from(Value::from(v)).ok()?))).collect())
            .unwrap_or_default()
    }
}
