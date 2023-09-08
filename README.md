<div id="top"></div>
<br />

<div align="center">
<a href="https://github.com/RaceCar/Embedded-End">
    <img src="https://avatars.githubusercontent.com/u/134273283?s=80" alt="Logo" width="auto" height="80" style="border-radius: 8px">
</a>

<h3 align="center">Embedded-End</h3>

![Project Version](https://img.shields.io/github/package-json/v/RaceCast/Acquisition?label=Version)&nbsp;
![Project License](https://img.shields.io/github/license/RaceCast/Acquisition?label=Licence)

  <p align="center">
    NodeJS scripts for on-board system data acquisition.
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
        <li><a href="#prerequisites">Prerequisites</a></li>
        <li><a href="#test-scripts">Test scripts</a></li>
        <li><a href="#deploy-on-embedded-system">Deploy on embedded system</a></li>
      </ul>
    </li>
    <li><a href="#author">Author</a></li>
  </ol>
</details>

## About The Project

Node.js scripts for acquiring and transmitting data from the various sensors mounted on the embedded system.

### Features

- Collects and processes data from the MPU6050 sensor
- Collects and processes data from the GPS sensor
- Send data from the embedded system to the [Back-End](https://github.com/RaceCast/Back-End):
    - MPU6050 data (accelerometer, gyroscope, rotation, temperature)
    - GPS data (latitude, longitude, altitude, speed)
    - Latency value between the embedded system and the API

### Tech Stack

- [Node.js](https://nodejs.org/)
    - [Socket.io](https://socket.io/)
    - [i2c-bus](https://www.npmjs.com/package/i2c-bus)
    - [i2c-mpu6050](https://www.npmjs.com/package/i2c-mpu6050)
- [socat](http://www.dest-unreach.org/socat/)
- [TypeScript](https://www.typescriptlang.org/)

<p align="right">(<a href="#top">back to top</a>)</p>

## Getting Started

### Prerequisites

- Install [Node.js](https://nodejs.org/) with [npm](https://www.npmjs.com/)

```bash
  curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash -
  sudo apt-get install -y nodejs
```

- Install [socat](http://www.dest-unreach.org/socat/)

```bash
  sudo apt-get install socat
```

- Create a `.env` file at the root of the project to set embedded system authentication key for
  the [Back-End](https://github.com/RaceCast/Back-End)

````env
  TOKEN="your-auth-key"
````

- Install dependencies

````bash
  npm install
````

- Lint and fix files

````bash
  npm run lint
````

- Compile TypeScript files

````bash
  npm run compile
````

### Test scripts

- Start individual scripts (sudo is required)

````bash
  node scripts/file.js # Replace file.js by the script you want to test
````

### Deploy on embedded system

- Start main script (sudo is required)

````bash
  npm run start
  # or
  node main.js
````

The script will automatically fetch and send data from the sensors to
the [Back-End](https://github.com/RaceCast/Back-End).

<p align="right">(<a href="#top">back to top</a>)</p>

## Author

[@Minarox](https://www.github.com/Minarox)

<p align="right">(<a href="#top">back to top</a>)</p>
