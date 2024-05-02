import {executeAT, logMessage} from "../utils";
import {GPS, Network, Signal} from "../types/modem";
import {LogLevel} from "../types/global";

// Count data to switch between commands
let data_count: number = 0;

/**
 * Show / Send data to the parent process
 *
 * @param {any} data - Data to send
 * @returns {void}
 */
function sendData(data: any): void {
    if (process.send) {
        process.send(data);
    } else {
        logMessage(JSON.stringify(data), LogLevel.DATA);
    }
}

/**
 * Get network, signal and GPS data from the modem
 *
 * @returns {Promise<void>}
 */
async function getModemDatas(): Promise<void> {
    // Get network data
    if (data_count === 0) {
        let network: Network | null = null;
        const networkResponse: string = await executeAT(`AT+QNWINFO`);

        if (networkResponse.trim().startsWith("+QNWINFO:")) {
            const data: Array<string> = networkResponse.trim().split(":")[1].trim().split(",");
            network = {
                type: data[0].slice(1, -1),
                channel: parseInt(data[3].slice(1, -6))
            };
        }

        sendData({network: network});
    }

    // Get signal data
    if (data_count === 1) {
        let signal: number | null = null;
        const signalResponse: string = await executeAT(`AT+CSQ`);

        if (signalResponse.trim().startsWith("+CSQ:")) {
            const data: Array<string> = signalResponse.trim().split(":")[1].trim().split(",");
            signal = parseInt(data[0]);
        }

        sendData({signal: signal});
    }

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
    }

    sendData({gps: gps});

    data_count++;
    if (data_count === 2) {
        data_count = 0;
    }

    // Recursive call
    setTimeout(getModemDatas);
}

setTimeout(getModemDatas);
