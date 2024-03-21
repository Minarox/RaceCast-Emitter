import {execute, executeAT, logMessage, wait} from "../utils";
import {LogType} from "../types/log";

/**
 * Restart audio services of the system
 * 
 * @returns {Promise<void>}
 */
async function setupAudio(): Promise<void> {
    await execute('systemctl --user restart pulseaudio.service');
    await execute('systemctl --user restart wireplumber.service');
    await execute('systemctl --user restart pipewire.service');
    await execute('systemctl --user restart pipewire-pulse.service');
}

/**
 * Set modem settings for serial communication
 * 
 * @returns {Promise<void>}
 */
async function setupModem(): Promise<void> {
    const echoResponse: string = await executeAT(`ATE0`);
    const scanModeResponse: string = await executeAT(`AT+QCFG="nwscanmode",0`);

    if (echoResponse.trim() !== 'OK' || scanModeResponse.trim() !== 'OK') {
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
        await setupAudio();
        await setupModem();
        await setupGPS();
        await waitForConnection();
    } catch (error) {
        logMessage(`Error setting up environment:\n${error}`, LogType.ERROR, true);
        process.exit(1);
    }
}

/**
 * Clear program environment
 * 
 * @returns {Promise<void>}
 */
export async function clearSetup(): Promise<void> {
    logMessage(`Clear environment...`, LogType.INFO, true);
    wait(800);

    try {
        await executeAT(`AT+QGPSEND`);
        await executeAT(`AT+QCFG="nwscanmode",0`);
    } catch (error) {
        logMessage(`Error clearing environment:\n${error}`, LogType.ERROR, true);
        process.exit(1);
    }
}
