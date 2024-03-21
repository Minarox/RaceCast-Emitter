import dotenv from 'dotenv';
import {clearSetup, setup} from "./src/scripts/setup";
import {logMessage} from "./src/utils";
import {LogLevel, Processes} from "./src/types/global";
import {fork} from 'child_process';

// Load environment variables from .env file
dotenv.config();

// Check if the script is running as root
if (!!(process.getuid && process.getuid() === 0) || !!(process.env['SUDO_UID'])) {
    logMessage(`This script must not be run as root. Exiting...`, LogLevel.ERROR);
    process.exit(1);
}

// Variables
let cleanupCalled: boolean = false;
const processes: Processes = {
    modem: null,
    sensor: null
};

/**
 * Launch and listen to sensor script
 *
 * @returns {void}
 */

function launchModem(): void {
    // Run the script
    processes.modem = fork(`${__dirname}/src/scripts/modem.${process.env['NODE_ENV'] === 'production' ? 'js' : 'ts'}`);

    // Fetch data
    processes.modem.on('message', (data): void => {
        logMessage(JSON.stringify(data), LogLevel.DATA);
    });

    // Restart if exit
    processes.modem.on('exit', (): void => {
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
    processes.sensor = fork(`${__dirname}/src/scripts/sensor.${process.env['NODE_ENV'] === 'production' ? 'js' : 'ts'}`);

    // Fetch data
    processes.sensor.on('message', (data): void => {
        logMessage(JSON.stringify(data), LogLevel.DATA);
    });

    // Restart if exit
    processes.sensor.on('exit', (): void => {
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
