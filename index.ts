import dotenv from 'dotenv';
import setup from "./src/scripts/setup";
import {logMessage, LogType} from "./src/utils";

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
    .then((): void => {
        console.log("Hello World!");
    });

/**
 * @description Cleanup and exit the script
 * @return {Promise<void>}
 */
async function cleanup(): Promise<void> {
    if (cleanupCalled) {
        return;
    }
    cleanupCalled = true;
    logMessage(`Cleanup and exiting...`, LogType.INFO, true);

    process.exit();
}

// Process events
["exit", "SIGINT", "SIGUSR1", "SIGUSR2", "uncaughtException"]
    .forEach((type: string): void => {
        process.on(type, cleanup);
    });
