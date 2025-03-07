<div id="top"></div>
<br />

<div align="center">
<a href="https://github.com/Minarox/RaceCast-Emitter">
    <img src="https://avatars.githubusercontent.com/u/71065703" alt="Logo" width="auto" height="80" style="border-radius: 8px">
</a>

<h3 align="center">RaceCast-Emitter</h3>

![Project Version](https://img.shields.io/github/package-json/v/Minarox/RaceCast-Emitter?label=Version)&nbsp;
![Project License](https://img.shields.io/github/license/Minarox/RaceCast-Emitter?label=Licence)

  <p align="center">
    Onboard autonomous IoT project to capture and transmit data and media stream from the race car.
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
      </ul>
    </li>
    <li>
      <a href="#getting-started">Getting Started</a>
      <ul>
        <li><a href="#deploy-on-embedded-system">Deploy on embedded system</a></li>
      </ul>
    </li>
    <li><a href="#author">Author</a></li>
  </ol>
</details>

## About The Project

Javascript app for acquiring and transmitting data and media stream from the various sensors mounted on the embedded system from the race car through cellular network.

### Features

- Fetch and parse modem (Network, GPS) and MPU6050 (temperature) datas
- Stream multiple media stream (audio and video) in realtime

### Tech Stack

- [Node](https://nodejs.org/)
- [pnpm](https://pnpm.io/)
- [TypeScript](https://www.typescriptlang.org/)
- [LiveKit](https://livekit.io/)
- [Puppeteer](https://pptr.dev/)
- [Chromium](https://www.chromium.org/)

<p align="right">(<a href="#top">back to top</a>)</p>

## Getting Started

This project is highly hardware / software dependant and as not been tested on other component expect mine :

- Raspberry Pi 5 (with Raspberry Pi OS)
- Quectel EC25 Modem (preconfigured in QMI mode, managed by ModemManager with "Orange" SIM card)
- GoPro Hero 12 Black
- V4L2 ready devices (including Elgato CamLink 4k and various Logitech webcam)
- MPU6050 sensor

### Deploy on embedded system

1. Install [pnpm](https://pnpm.io/) and [Chromium](https://www.chromium.org/) on the host

2. Clone the project and install dependencies :

```bash
git clone https://github.com/Minarox/RaceCast-Emitter
cd RaceCast-Emitter
pnpm install
```

3. Create `.env` file from `.env.example` at the root of the project with real [LiveKit](https://livekit.io/) server.

4. Run the app :

```bash
pnpm dev
```

Or with auto-restart if crash occurs at some point:
```bash
pnpm start
```

<p align="right">(<a href="#top">back to top</a>)</p>

## Author

[@Minarox](https://www.github.com/Minarox)

<p align="right">(<a href="#top">back to top</a>)</p>
