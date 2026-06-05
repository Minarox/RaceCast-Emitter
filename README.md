<div id="top"></div>
<br />

<div align="center">
<a href="https://github.com/Minarox/RaceCast-Emitter">
    <img src="https://avatars.githubusercontent.com/u/71065703" alt="Logo" width="auto" height="80" style="border-radius: 8px">
</a>

<h3 align="center">RaceCast</h3>

![Go Version](https://img.shields.io/github/go-mod/go-version/Minarox/RaceCast-Emitter?label=Go)&nbsp;
![Project License](https://img.shields.io/github/license/Minarox/RaceCast-Emitter?label=Licence)

  <p align="center">
    Onboard autonomous IoT project to capture and transmit data and media stream from a race car.
    <br />
    <a href="https://racecast.minarox.fr/"><strong>racecast.minarox.fr »</strong></a>
  </p>
</div>
<br />

<details>
  <summary>Table of Contents</summary>
  <ol>
    <li>
      <a href="#about-the-project">About The Project</a>
      <ul>
        <li><a href="#features">Features</a></li>
        <li><a href="#tech-stack">Tech Stack</a></li>
        <li><a href="#hardware">Hardware</a></li>
        <li><a href="#configuration">Configuration</a></li>
      </ul>
    </li>
    <li><a href="#author">Author</a></li>
  </ol>
</details>

## About The Project

> ⚠️ **Important note:**
> This project has a strong dependency on the specific hardware and software configurations used. It is provided for reference only for those who wish to create a similar system.

[Go](https://go.dev/) script for acquiring and transmitting data and media stream from the various sensors mounted on the embedded system from the race car through cellular network.

### Features

- Collecting position, connectivity and battery state from modem and ups
- Updating [LiveKit](https://livekit.io/) room metadata with latest sensors data
- Creating and publishing audio, video and data tracks to [LiveKit](https://livekit.io/) server

### Tech Stack

- [Go](https://go.dev/)
- [LiveKit](https://livekit.io/)
- [ModemManager](https://modemmanager.org/)

### Hardware

- [Waveshare Jetson Orin NX Development Kit](https://www.waveshare.com/jetson-orin-nx-16g-dev-kit.htm?sku=24475)
- [Waveshare RM520N-GL Hat](https://www.waveshare.com/rm520n-gl-5g-for-jetson-orin.htm)
- [Waveshare UPS Power Module](https://www.waveshare.com/UPS-Power-Module-C.htm)
- Various UVC cameras and microphones

### Configuration

A `.env` file must be created with the information provided in `.env.example`.
All fields are required.

#### ModemManager

Grant the desired user the necessary permissions to partially control [ModemManager](https://modemmanager.org/).
This step is required if you want to use the [Go](https://go.dev/) script without running it with `sudo`.

```bash
sudo nano /etc/polkit-1/localauthority/50-local.d/50-modemmanager.pkla
```

```bash
[Allow mmcli]
Identity=unix-user:username
Action=org.freedesktop.ModemManager1.Device.Control
ResultAny=yes
ResultInactive=yes
ResultActive=yes

[Allow mmcli location]
Identity=unix-user:username
Action=org.freedesktop.ModemManager1.Location
ResultAny=yes
ResultInactive=yes
ResultActive=yes
```

```bash
sudo systemctl restart ModemManager
```

## Author

[@Minarox](https://www.github.com/Minarox)

<p align="right">(<a href="#top">back to top</a>)</p>
