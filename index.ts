import dotenv from 'dotenv';
import {clearSetup, setup} from "./src/scripts/setup";
import {logMessage} from "./src/utils";
import {LogType, Processes} from "./src/types/global";
import { fork } from 'child_process';

// Load environment variables from .env file
dotenv.config();

// Check if the script is running as root
if (!!(process.getuid && process.getuid() === 0) || !!(process.env['SUDO_UID'])) {
    logMessage(`This script must not be run as root. Exiting...`, LogType.ERROR, true);
    process.exit(1);
}

// Variables
let cleanupCalled: boolean = false;
const processes: Processes = {
    modem: null,
    sensor: null
}

/**
 * Launch and listen to sensor script
 * 
 * @returns {void}
 */
function launchModem(): void {
    // Run the script
    if (process.env['NODE_ENV'] !== "production") {
        processes.modem = fork("npx", ["ts-node", "src/scripts/modem.ts"], { stdio: "pipe" });
    } else {
        processes.modem = fork("node", ["src/scripts/modem.js"], { stdio: "pipe" });
    }

    // Fetch data
    processes.modem.on('message', (data) => {
        console.log(data);
    });

    // Restart if exit
    processes.modem.on('exit', () => {
        processes.sensor = null;

        if (!cleanupCalled) {
            setTimeout(launchSensor, 1000);
        }
    });
}

/**
 * Launch and listen to sensor script
 * 
 * @returns {void}
 */
function launchSensor(): void {
    // Run the script
    if (process.env['NODE_ENV'] !== "production") {
        processes.sensor = fork("npx", ["ts-node", "src/scripts/sensor.ts"], { stdio: "pipe" });
    } else {
        processes.sensor = fork("node", ["src/scripts/sensor.js"], { stdio: "pipe" });
    }

    // Fetch data
    processes.sensor.on('message', (data) => {
        console.log(data);
    });

    // Restart if exit
    processes.sensor.on('exit', () => {
        processes.sensor = null;

        if (!cleanupCalled) {
            setTimeout(launchSensor, 1000);
        }
    });
}

// Setup environment and run scripts
setup()
    .then(async (): Promise<void> => {
        logMessage(`Launching scripts...`);

        launchModem();
        launchSensor();

        console.log('Hello World!');
    });

/**
 * Cleanup the program before exit
 * 
 * @returns {Promise<void>}
 */
async function cleanUp(): Promise<void> {
    if (cleanupCalled) {
        return;
    }
    cleanupCalled = true;

    // Kill processes
    for (const key in processes) {
        if (processes[key]) {
            await processes[key].kill();
        }
    }

    // Clear environment
    await clearSetup();

    process.exit();
}

// Process events
["exit", "SIGINT", "SIGUSR1", "SIGUSR2", "uncaughtException"]
    .forEach((type: string): void => {
        process.on(type, cleanUp);
    });
