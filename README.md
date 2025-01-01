<div id="top"></div>
<br />

<div align="center">
<a href="https://github.com/RaceCast/Emitter">
    <img src="https://avatars.githubusercontent.com/u/134273283?s=80" alt="Logo" width="auto" height="80" style="border-radius: 8px">
</a>

<h3 align="center">Emitter</h3>

![Project Version](https://img.shields.io/github/package-json/v/RaceCast/Emitter?label=Version)&nbsp;
![Project License](https://img.shields.io/github/license/RaceCast/Emitter?label=Licence)

  <p align="center">
    Onboard autonomous IoT project to capture and transmit data and media stream from the race car.
    <br />
    <a href="https://rallye.minarox.fr/"><strong>rallye.minarox.fr »</strong></a>
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

- Fetch and parse modem datas (Network, GPS)
- Stream multiple media stream (audio and video) in realtime

### Tech Stack

- [Bun](https://bun.sh/)
- [TypeScript](https://www.typescriptlang.org/)
- [LiveKit](https://github.com/livekit/server-sdk-js)
- [Puppeteer](https://pptr.dev/)

<p align="right">(<a href="#top">back to top</a>)</p>

## Getting Started

This project is highly hardware / software dependant and as not been tested on other component expect mine :

- Raspberry Pi 5 (with Raspberry Pi OS)
- Quectel EC25 Modem (preconfigured in QMI mode, with an "Orange" SIM card)
- GoPro Hero 12 Black
- Elgato CamLink 4K

### Deploy on embedded system

1. Clone the project and install dependencies :

```bash
git clone https://github.com/Minarox/RaceCast-Emitter
cd RaceCast-Emitter
bun install
```

2. Create `.env` file at the root of the project with these variables:

```dotenv
LIVEKIT_TLS="true"
LIVEKIT_DOMAIN="example.com"
LIVEKIT_KEY="key"
LIVEKIT_SECRET="secret"
LIVEKIT_ROOM="room"
```

3. Build TypeScript files :

```bash
bun run build
```

4. Run the app :

```bash
bun run start
```

The app automaticaly setting up environment and start needed scripts.

<p align="right">(<a href="#top">back to top</a>)</p>

## Author

[@Minarox](https://www.github.com/Minarox)

<p align="right">(<a href="#top">back to top</a>)</p>
