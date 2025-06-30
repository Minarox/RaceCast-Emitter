<div id="top"></div>
<br />

<div align="center">
<a href="https://github.com/Minarox/RaceCast-Emitter">
    <img src="https://avatars.githubusercontent.com/u/71065703" alt="Logo" width="auto" height="80" style="border-radius: 8px">
</a>

<h3 align="center">RaceCast-Emitter</h3>

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

- Collecting data from the modem, GPS, and sensors by executing [ModemManager](https://modemmanager.org/) cli commands
- Updating [LiveKit](https://livekit.io/) room metadata at regular intervals when changes are detected
- Creating and controlling an optimized [Chromium](https://www.chromium.org/) instance
- Capturing and sending various streams from [Chromium](https://www.chromium.org/) to the [LiveKit](https://livekit.io/) server

### Tech Stack

- [Go](https://go.dev/)
- [Chromium](https://www.chromium.org/)
- [LiveKit](https://livekit.io/)
- [ModemManager](https://modemmanager.org/)

### Hardware

- [Raspberry Pi 5](https://www.raspberrypi.com/products/raspberry-pi-5/) (8 Go)
- [SixFab 4G/LTE Cellular Modem Kit](https://sixfab.com/product/raspberry-pi-4g-lte-modem-kit/)
- [Battery UPS HAT](https://www.dfrobot.com/product-2840.html?srsltid=AfmBOooD28ApNQEqDY-Vkc-WCCOt0VZ2mOPo3arpIw6eJ0-mFeeHss7Z)
- MPU6050 sensor
- Various UVC Webcam, including Elgato Cam Link 4K

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
