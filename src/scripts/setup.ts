import {executeAT, logMessage, wait} from "../utils";
import {LogLevel} from "../types";

/**
 * Restart audio services of the system
 *
 * @returns {Promise<void>}
 */
async function setupAudio(): Promise<void> {
    //await execute('systemctl --user restart pulseaudio.service');
    //await execute('systemctl --user restart wireplumber.service');
    //await execute('systemctl --user restart pipewire.service');
    //await execute('systemctl --user restart pipewire-pulse.service');
}

/**
 * Set modem settings for serial communication
 *
 * @returns {Promise<void>}
 */
async function setupModem(): Promise<void> {
    await executeAT(`ATE0`).catch();
    await executeAT(`AT+COPS=0,0`).catch();
    await executeAT(`AT+QCFG="roamservice",2,1`).catch();
    await executeAT(`AT+CGATT=1`).catch();
    await executeAT(`AT+CGACT=1,1`).catch();
    await executeAT(`AT+CGQREQ="IP",1,1,5,9,31`).catch();
    await executeAT(`AT+CGQMIN="IP",2,3,3,7,31`).catch();
    await executeAT(`AT+CGEQREQ="IP",1,5760,42200,1024,4096,0,0,"0E0","0E0",3,0,0,1,1`).catch();
    await executeAT(`AT+CGEQMIN="IP",1,0,0,0,0,0,0,"0E0","0E0",3,0,0,0,0`).catch();
}

/**
 * Loop until modem establish a connection to the network
 *
 * @returns {Promise<true>} True when the modem is connected
 */
async function waitForConnection(): Promise<true> {
    const registeredResponse: string = await executeAT(`AT+CREG?`);
    const connectedResponse: string = await executeAT(`AT+CGATT?`);

    if (!registeredResponse.trim().startsWith('+CREG: 0,1') || !connectedResponse.trim().startsWith('+CGATT: 1')) {
        wait(500);
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

        logMessage(`Wait internet connection...`);
        wait(2000);
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
    wait(1000);

    try {
        // Disable GPS
        await executeAT(`AT+QGPSEND`);
    } catch (error) {
        logMessage(`Error clearing environment:\n${error}`, LogLevel.ERROR);
        process.exit(1);
    }
}
