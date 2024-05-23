import dotenv from 'dotenv';
import {clearSetup, setup} from "./src/scripts/setup";
import {logMessage} from "./src/utils";
import {LogLevel, Processes} from "./src/types/global";
import {fork} from 'child_process';

// Check if the script is running as root
if (!!(process.getuid && process.getuid() === 0) || !!(process.env['SUDO_UID'])) {
    logMessage(`This script must not be run as root. Exiting...`, LogLevel.ERROR);
    process.exit(1);
}

// Load environment variables from .env file
dotenv.config();

// Variables
const fileType: string = process.env['NODE_ENV'] === 'production' ? 'js' : 'ts';
const launchArgs: string[] = [];
let cleanupCalled: boolean = false;
const processes: Processes = {
    modem: null,
    sensor: null,
    stream: null
};

process.argv.slice(2).forEach((arg: string): void => {
    launchArgs.push(arg);
});

/**
 * Launch and listen to sensor script
 *
 * @returns {void}
 */
function launchModem(): void {
    logMessage(`Launching Modem script...`, LogLevel.INFO);
    processes.modem = fork(`${__dirname}/src/scripts/modem.${fileType}`, launchArgs);

    // Fetch data
    processes.modem?.on('message', (data: any): void => {
        processes.stream?.send(data);
    });

    // Restart if exit
    processes.modem?.on('exit', (reason: string): void => {
        logMessage(`Modem script exiting${reason ? ` :\n${reason}` : '.'}`, LogLevel.WARNING);
        processes.modem = null;

        setTimeout(() => {
            if (!cleanupCalled) {
                launchModem();
            }
        }, 1000);
    });
}

/**
 * Launch and listen to sensor script
 *
 * @returns {void}
 */
function launchSensor(): void {
    logMessage(`Launching Sensor script...`, LogLevel.INFO);
    processes.sensor = fork(`${__dirname}/src/scripts/sensor.${fileType}`, launchArgs);

    // Fetch data
    processes.sensor?.on('message', (data: any): void => {
        processes.stream?.send(data);
    });

    // Restart if exit
    processes.sensor?.on('exit', (reason: string): void => {
        logMessage(`Sensor script exiting${reason ? ` :\n${reason}` : '.'}`, LogLevel.WARNING);
        processes.sensor = null;

        setTimeout(() => {
            if (!cleanupCalled) {
                launchSensor();
            }
        }, 1000);
    });
}

/**
 * Launch and listen to stream script
 *
 * @returns {void}
 */
function launchStream(): void {
    logMessage(`Launching Stream script...`, LogLevel.INFO);
    processes.stream = fork(`${__dirname}/src/scripts/stream.${fileType}`, launchArgs);

    // Fetch data
    processes.stream?.on('message', (data: any): void => {
        const message: string = JSON.stringify(data) || '';
        if (message === '{"name":"DOMException"}') {
            processes.stream?.kill();
        }
        logMessage(message, LogLevel.DATA);
    });

    // Restart if exit
    processes.stream?.on('exit', (reason: string): void => {
        logMessage(`Stream script exiting${reason ? ` :\n${reason}` : '.'}`, LogLevel.WARNING);
        processes.stream = null;

        setTimeout(() => {
            if (!cleanupCalled) {
                launchStream();
            }
        }, 1000);
    });
}

// Setup environment and run scripts
setup()
    .then(async (): Promise<void> => {
        logMessage(`Starting main program...`);

        launchModem();
        launchSensor();
        launchStream();
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
