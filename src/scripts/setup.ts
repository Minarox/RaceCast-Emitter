import {exec} from "child_process";
import {ExecException} from "node:child_process";
import {logMessage, LogType} from "../utils";

/**
 * @function execWrapper
 * @description Wrapper for exec function
 * @param {string} command - Command to execute
 * @return {Promise<string>} output of the command
 */
function execWrapper(command: string): Promise<string> {
    return new Promise((resolve): void => {
        exec(command, (_error: ExecException | null, stdout: string): void => {
            setTimeout(() => resolve(stdout), 100);
        });
    });
}

/**
 * @function restartAudio
 * @description Restart audio services
 * @return {Promise<void>}
 */
async function restartAudio(): Promise<void> {
    await execWrapper('/etc/init.d/alsa-utils restart');
    await execWrapper('systemctl --user restart wireplumber.service');
    await execWrapper('systemctl --user restart pipewire.service');
}

/**
 * @function setModemSettings
 * @description Set modem settings for serial communication
 * @return {Promise<void>}
 */
async function setModemSettings(): Promise<void> {
    const echoResponse: string = await execWrapper(`echo 'ATE0' | socat - /dev/ttyUSB2,crnl`);
    const scanModeResponse: string = await execWrapper(`echo 'AT+QCFG="nwscanmode",0' | socat - /dev/ttyUSB2,crnl`);

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
    const connectedResponse: string = await execWrapper(`echo 'AT+CGATT?' | socat - /dev/ttyUSB2,crnl`);
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
