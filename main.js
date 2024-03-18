import { config } from "dotenv";
import path from 'path';
import { fileURLToPath } from 'url';
import { getBrowser, launchLiveKit, sendData } from "./src/livekit.js";
import { spawn } from "node:child_process";

let cleanupCalled = false;
let processes = {
    sensor: null,
    modem: null
};

// Load .env file and check properties
config({ path: `${path.dirname(fileURLToPath(import.meta.url))}/.env` });
const requiredEnvVars = ["API_URL", "API_WS_URL", "API_KEY", "API_SECRET", "API_ROOM"];
const missingEnvVar = requiredEnvVars.find(envVar => !process.env[envVar]);

if (missingEnvVar) {
    if (process.stdout.isTTY) {
        process.stderr.write(`\r\nERROR: Missing ${missingEnvVar} property in .env file.`);
    }
    process.exit(1);
}

// Launch sensor script
function runSensor() {
    // Run the script
    processes.sensor = spawn("node", ["src/sensor.js"], { stdio: "pipe" });

    // Send data to the users
    processes.sensor.stdout.on("data", (data) => sendData({ sensor: JSON.parse(data) }));

    // Restart the script if it crashes or exits
    ["exit", "error"].forEach((type) => {
        processes.sensor.on(type, () => {
            sendData({ sensor: null }, false);
            processes.sensor = null;
            if (!cleanupCalled) setTimeout(runSensor, 2000);
        });
    });
}

// Launch modem script
function runModem() {
    // Run the script
    processes.modem = spawn("node", ["src/modem.js"], { stdio: "pipe" });

    // Send data to the users
    processes.modem.stdout.on("data", (data) => sendData({ modem: JSON.parse(data) }));

    // Restart the script if it crashes or exits
    ["exit", "error"].forEach((type) => {
        processes.modem.on(type, () => {
            sendData({ modem: null }, false);
            processes.modem = null;
            if (!cleanupCalled) setTimeout(runModem, 2000);
        });
    });
}

// Run scripts
// runSensor();
runModem();
launchLiveKit().then();

// Cleanup on exit
async function cleanup() {
    if (cleanupCalled) {
        return;
    }
    cleanupCalled = true;

    if (process.stdout.isTTY) {
        process.stdout.write("\r\nCleanup and exiting...\r\n");
    }

    // Kill all processes
    getBrowser().then(browser => browser.close());
    for (const key in processes) {
        if (processes[key]) {
            await processes[key].kill();
        }
    }

    process.exit();
}

["exit", "SIGINT", "SIGUSR1", "SIGUSR2", "uncaughtException"]
    .forEach((type) => {
        process.on(type, cleanup);
    });
