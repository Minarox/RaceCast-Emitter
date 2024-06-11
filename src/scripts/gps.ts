import {executeAT, logMessage, asProcessArg, getRandomInt, getRandomFloat, updateRoomMetadata, wait} from "../utils";
import {LogLevel, GPS} from "../types";

/**
 * @description Show / Send data to the parent process
 * @param {any} data - Data to send
 * @returns {void}
 */
function sendData(data: any): void {
    if (process.send) {
        updateRoomMetadata(data)
    } else {
        logMessage(JSON.stringify(data), LogLevel.DATA);
    }
}

/**
 * @description Setup GPS data format and enable GPS
 * @returns {Promise<void>}
 */
async function setupGPS(): Promise<void> {
    await executeAT(`ATE0`);
    if ((await executeAT(`AT+QGPS?`)).trim().startsWith("+QGPS: 0")) {
        // Set data format and enable GPS
        await executeAT(`AT+QGPSCFG=\"nmeasrc\",1`);
        await executeAT(`AT+QGPS=1`);
        wait(500);
        setTimeout(getGPSDatas);
    } else {
        // Disable GPS and re-init
        await executeAT(`AT+QGPSEND`);
        wait(500);
        await setupGPS();
    }
}

/**
 * @description Get network, signal and GPS data from the modem
 * @returns {Promise<void>}
 */
async function getGPSDatas(): Promise<void> {
    // Get GPS data
    let gps: GPS | null = null;
    const gpsResponse: string = await executeAT(`AT+QGPSLOC=1`);

    if (gpsResponse.trim().startsWith("+QGPSLOC:")) {
        const data: Array<string> = gpsResponse.trim().split(":")[1].trim().split(",");
        gps = {
            latitude: parseFloat(data[1].slice(0, 2)) + parseFloat(data[1].slice(2)) / 60,
            longitude: parseFloat(data[3].slice(0, 3)) + parseFloat(data[3].slice(3)) / 60,
            altitude: parseFloat(data[6]),
            speed: parseFloat(data[9]),
        };

        sendData({ gps: gps });
    }

    // Recursive call
    setTimeout(getGPSDatas, 500);
}

if (asProcessArg('fake-devices')) {
    setInterval((): void => {
        sendData({
            gps: {
                latitude: getRandomFloat(48.858, 48.859, 5),
                longitude: getRandomFloat(2.294, 2.295, 5),
                altitude: getRandomInt(0, 300),
                speed: getRandomFloat(0, 120, 2),
            }
        });
    }, 1000);
} else {
    setTimeout(setupGPS);
}
