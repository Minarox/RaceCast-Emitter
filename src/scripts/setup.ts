import {executeAT, logMessage, wait} from "../utils";
import {LogLevel} from "../types/global";

/**
 * Set modem settings for serial communication
 *
 * @returns {Promise<void>}
 */
async function setupModem(): Promise<void> {
    await executeAT(`ATE0`).catch();
    const scanModeResponse: string = await executeAT(`AT+QCFG="nwscanmode",0`);

    if (scanModeResponse.trim() !== 'OK') {
        throw new Error(`Unable to set modem settings`);
    }
}

/**
 * Setup GPS data format and enable GPS
 *
 * @returns {Promise<void>}
 */
async function setupGPS(): Promise<void> {
    const gpsState: string = await executeAT(`AT+QGPS?`);
    if (gpsState.trim().startsWith("+QGPS: 0")) {
        // Set data format and enable GPS
        await executeAT(`AT+QGPSCFG=\"nmeasrc\",1`);
        await executeAT(`AT+QGPS=1`);
    } else {
        // Disable GPS and re-init
        await executeAT(`AT+QGPSEND`);
        await setupGPS();
    }
}

/**
 * Loop until modem establish a connection to the network
 *
 * @returns {Promise<true>} True when the modem is connected
 */
async function waitForConnection(): Promise<true> {
    const connectedResponse: string = await executeAT(`AT+CGATT?`);

    if (!connectedResponse.trim().startsWith('+CGATT: 1')) {
        await waitForConnection();
    }
    return true;
}

/**
 * Setup program environment
 *
 * @returns {Promise<void>}
 */
export async function setup(): Promise<void> {
    logMessage(`Setup environment...`);

    try {
        await setupModem();
        await setupGPS();

        logMessage(`Wait internet connection...`);
        await waitForConnection();
    } catch (error) {
        logMessage(`Error setting up environment:\n${error}`, LogLevel.ERROR);
        process.exit(1);
    }
}

/**
 * Clear program environment
 *
 * @returns {Promise<void>}
 */
export async function clearSetup(): Promise<void> {
    logMessage(`Clear environment...`);
    wait(800);

    try {
        // Disable GPS
        await executeAT(`AT+QGPSEND`);
        // Search all network type
        await executeAT(`AT+QCFG="nwscanmode",0`);
    } catch (error) {
        logMessage(`Error clearing environment:\n${error}`, LogLevel.ERROR);
        process.exit(1);
    }
}
