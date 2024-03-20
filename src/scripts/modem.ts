import {execute} from "../utils";
import {GPS, Network, Signal, Type} from "../types/modem";

// Count data to switch between commands
let data_count: number = 0;

/**
 * @function atCommand
 * @description Execute AT command and return response
 * @param {string} atCommand - AT commad to execute
 * @param {boolean} exitOnError - Exit process on error
 * @returns {Promise<string>} modem response
 */
async function atCommand(atCommand: string, exitOnError: boolean = true): Promise<string> {
    const command: string = `echo '${atCommand}' | socat - /dev/ttyUSB2,crnl`;
    return new Promise(async (resolve): Promise<void> => {
        await execute(command)
            .then((response) => resolve(response))
            .catch(() => {
                if (exitOnError) {
                    process.exit(1);
                }
            });
    })
}

/**
 * @function getDatas
 * @description Get network, signal and GPS data from the modem
 * @returns {Promise<void>}
 */
async function getDatas(): Promise<void> {
    // Get network data
    if (data_count === 0) {
        const networkResponse: string = await atCommand(`AT+QNWINFO`, false);
        if (networkResponse.trim().startsWith("+QNWINFO:")) {
            const data: Array<string> = networkResponse.trim().split(":")[1].trim().split(",");
            const network: Network = {
                access: data[0].slice(1, -1),
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
    if (data_count === 3) {
        const signalResponse: string = await atCommand(`AT+CSQ`, false);
        if (signalResponse.trim().startsWith("+CSQ:")) {
            const data: Array<string> = signalResponse.trim().split(":")[1].trim().split(",");
            const signal: Signal = {
                signal: parseInt(data[0]),
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
    const gpsResponse = await atCommand(`AT+QGPSLOC=1`, false);
    if (gpsResponse.trim().startsWith("+QGPSLOC:")) {
        const data = gpsResponse.trim().split(":")[1].trim().split(",");
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
    if (data_count === 6) {
        data_count = 0;
    }

    // Recursive call
    setTimeout(getDatas);
}

/**
 * @function initGPS
 * @description Prepare GPS module
 * @returns {Promise<void>}
 */
async function initGPS(): Promise<void> {
    // Set data format
    await atCommand(`AT+QGPSCFG=\"nmeasrc\",1`);

    const gpsState: string = await atCommand(`AT+QGPS?`);
    if (gpsState.trim().startsWith("+QGPS: 0")) {
        // Enable GPS
        await atCommand(`AT+QGPS=1`);

        // Get GPS location
        await getDatas();
    } else {
        // Disable GPS and re-init
        await atCommand(`AT+QGPSEND`);
        await initGPS();
    }
}

// Init communication with the modem
/**
 * @function start
 * @description Start the modem script
 * @returns {Promise<void>}
 */
async function start(): Promise<void> {
    // Disable duplicate echo
    await atCommand(`ATE0`);

    // Init GPS
    await initGPS();
}

start().then();
