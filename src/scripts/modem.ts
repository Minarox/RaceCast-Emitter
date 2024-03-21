import {executeAT} from "../utils";
import {GPS, Network, Signal, Type} from "../types/modem";

// Count data to switch between commands
let data_count: number = 0;

/**
 * Get network, signal and GPS data from the modem
 * 
 * @returns {Promise<void>}
 */
export default async function getModemDatas(): Promise<void> {
    // Get network data
    if (data_count === 0) {
        const networkResponse: string = await executeAT(`AT+QNWINFO`);
        if (networkResponse.trim().startsWith("+QNWINFO:")) {
            const data: Array<string> = networkResponse.trim().split(":")[1].trim().split(",");
            const network: Network = {
                type: data[0].slice(1, -1),
                band: data[2].slice(1, -1),
                channel: parseInt(data[3].slice(1, -6))
            };

            process.stdout.write(
                JSON.stringify({Network: network})
            );
        } else {
            process.stderr.write(Type.NETWORK);
        }
    }

    // Get signal data
    if (data_count === 1) {
        const signalResponse: string = await executeAT(`AT+CSQ`);
        if (signalResponse.trim().startsWith("+CSQ:")) {
            const data: Array<string> = signalResponse.trim().split(":")[1].trim().split(",");
            const signal: Signal = {
                strength: parseInt(data[0]),
                error_rate: parseInt(data[1])
            };

            process.stdout.write(
                JSON.stringify({Signal: signal})
            );
        } else {
            process.stderr.write(Type.SIGNAL);
        }
    }

    // Get GPS data
    const gpsResponse: string = await executeAT(`AT+QGPSLOC=1`);
    if (gpsResponse.trim().startsWith("+QGPSLOC:")) {
        const data: Array<string> = gpsResponse.trim().split(":")[1].trim().split(",");
        const gps: GPS = {
            latitude: parseFloat(data[1].slice(0, 2)) + parseFloat(data[1].slice(2)) / 60,
            longitude: parseFloat(data[3].slice(0, 3)) + parseFloat(data[3].slice(3)) / 60,
            altitude: parseFloat(data[6]),
            speed: parseFloat(data[9]),
        };
        process.stdout.write(
            JSON.stringify({GPS: gps})
        );
    } else {
        process.stderr.write(Type.GPS);
    }

    data_count++;
    if (data_count === 2) {
        data_count = 0;
    }
}
