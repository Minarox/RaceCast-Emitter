import dotenv from 'dotenv';
import {clearSetup, setup} from "./src/scripts/setup";
import {logMessage, LogType} from "./src/utils";
import {getDatas} from "./src/scripts/modem";

// Load environment variables from .env file
dotenv.config();

// Check if the script is running as root
if (!!(process.getuid && process.getuid() === 0) || !!(process.env['SUDO_UID'])) {
    logMessage(`This script must not be run as root. Exiting...`, LogType.ERROR, true);
    process.exit(1);
}

// Variables
let cleanupCalled: boolean = false;

// Setup environment and run scripts
setup()
    .then(async (): Promise<void> => {
        logMessage(`Launching scripts...`);

        // Modem script
        getDatas();

        console.log('Hello World!');
    });

/**
 * @function cleanUp
 * @description Cleanup and exit the script
 * @returns {Promise<void>}
 */
async function cleanUp(): Promise<void> {
    if (cleanupCalled) {
        return;
    }
    cleanupCalled = true;

    // Clear environment
    await clearSetup();

    process.exit();
}

// Process events
["exit", "SIGINT", "SIGUSR1", "SIGUSR2", "uncaughtException"]
    .forEach((type: string): void => {
        process.on(type, cleanUp);
    });
