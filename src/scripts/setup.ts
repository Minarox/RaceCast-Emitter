import {execute, logMessage, LogType} from "../utils";

/**
 * @function restartAudio
 * @description Restart audio services
 * @return {Promise<void>}
 */
async function restartAudio(): Promise<void> {
    await execute('/etc/init.d/alsa-utils restart');
    await execute('systemctl --user restart wireplumber.service');
    await execute('systemctl --user restart pipewire.service');
}

/**
 * @function setModemSettings
 * @description Set modem settings for serial communication
 * @return {Promise<void>}
 */
async function setModemSettings(): Promise<void> {
    const echoResponse: string = await execute(`echo 'ATE0' | socat - /dev/ttyUSB2,crnl`);
    const scanModeResponse: string = await execute(`echo 'AT+QCFG="nwscanmode",0' | socat - /dev/ttyUSB2,crnl`);

    if (echoResponse.trim() !== 'OK' || scanModeResponse.trim() !== 'OK') {
        logMessage(`Unable to set modem settings. Exiting...`, LogType.ERROR, true);
        process.exit(1);
    }
}

/**
 * @function waitForConnection
 * @description Wait for modem connection
 * @return {Promise<true>} true when the modem is connected
 */
async function waitForConnection(): Promise<true> {
    const connectedResponse: string = await execute(`echo 'AT+CGATT?' | socat - /dev/ttyUSB2,crnl`);
    if (!connectedResponse.trim().startsWith('+CGATT: 1')) {
        await waitForConnection();
    }
    return true;
}

/**
 * @function setup
 * @description Setup program environment
 * @return {Promise<void>}
 */
export default async function setup(): Promise<void> {
    logMessage(`Setup environment...`);

    await restartAudio();
    await setModemSettings();
    await waitForConnection();

    logMessage(`Launching program...`);
}
