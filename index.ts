import dotenv from 'dotenv';
import {clearSetup, setup} from "./src/scripts/setup";
import {logMessage, executeAT, asProcessArg} from "./src/utils";
import {LogLevel, Processes} from "./src/types";
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
    gps: null,
    livekit: null
};

process.argv.slice(2).forEach((arg: string): void => {
    launchArgs.push(arg);
});

/**
 * @description Launch and listen to GPS script
 * @returns {void}
 */
function launchGPS(): void {
    logMessage(`Launching GPS script...`, LogLevel.INFO);
    processes.gps = fork(`${__dirname}/src/scripts/gps.${fileType}`, launchArgs);

    // Restart if exit
    processes.gps?.on('exit', async (reason: string): Promise<void> => {
        logMessage(`GPS script exiting${reason ? ` :\n${reason}` : '.'}`, LogLevel.WARNING);
        processes.gps = null;

        // Disable GPS
        await executeAT(`AT+QGPSEND`);

        setTimeout((): void => {
            if (!cleanupCalled) {
                launchGPS();
            }
        }, 1000);
    });
}

/**
 * @description Launch and listen to livekit script
 * @returns {void}
 */
function launchLiveKit(): void {
    logMessage(`Launching LiveKit script...`, LogLevel.INFO);
    processes.livekit = fork(`${__dirname}/src/scripts/livekit.${fileType}`, launchArgs);

    // Fetch data
    processes.livekit?.on('message', (data: any): void => {
        const message: string = JSON.stringify(data) || '';
        if (message === '{"name":"DOMException"}' || message === '{"name":"un"}' || message === '{"name":"TypeError"}') {
            processes.livekit?.kill();
        }
        logMessage(message, LogLevel.DATA);
    });

    // Restart if exit
    processes.livekit?.on('exit', (reason: string): void => {
        logMessage(`LiveKit script exiting${reason ? ` :\n${reason}` : '.'}`, LogLevel.WARNING);
        processes.livekit = null;

        setTimeout((): void => {
            if (!cleanupCalled) {
                launchLiveKit();
            }
        }, 1000);
    });
}

// Setup environment and run scripts
setup()
    .then(async (): Promise<void> => {
        logMessage(`Starting main program...`);

        if (!asProcessArg('no-gps')) {
            launchGPS();
        }
        launchLiveKit();
    });

/**
 * @description Cleanup the program before exit
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
