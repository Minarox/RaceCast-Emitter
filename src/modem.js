import { exec } from "child_process";

let data_count = 0;

// Execute AT command and return response
// Note : `sudo chmod a+rw /dev/ttyUSB2` to allow access to the modem without root
function execWrapper(command, exit_on_error = true) {
    return new Promise(async (resolve) => {
        await exec(
            `echo '${command}' | socat - /dev/ttyUSB2,crnl`,
            (error, stdout) => {
                if (exit_on_error && error) process.exit(1);
                else resolve(stdout);
            });
    });
}

// Get network, signal and GPS data
async function getDatas() {
    // Get network data
    if (data_count === 0) {
        const network_response = await execWrapper(`AT+QNWINFO`, false);
        if (network_response.trim().startsWith("+QNWINFO:")) {
            const data = network_response.trim().split(":")[1].trim().split(",");
            process.stdout.write(
                JSON.stringify({
                    Network: {
                        access: data[0].slice(1, -1),
                        band: data[2].slice(1, -1),
                        channel: parseInt(data[3].slice(1, -6))
                    }
                })
            );
        } else process.stderr.write("Network");
    }

    // Get signal data
    if (data_count === 3) {
        const signal_response = await execWrapper(`AT+CSQ`, false);
        if (signal_response.trim().startsWith("+CSQ:")) {
            const data = signal_response.trim().split(":")[1].trim().split(",");
            process.stdout.write(
                JSON.stringify({
                    Signal: {
                        signal: parseInt(data[0]),
                        error_rate: parseInt(data[1])
                    }
                })
            );
        } else process.stderr.write("Signal");
    }

    // Get GPS data
    const gps_response = await execWrapper(`AT+QGPSLOC=1`, false)
    if (gps_response.trim().startsWith("+QGPSLOC:")) {
        const data = gps_response.trim().split(":")[1].trim().split(",");
        process.stdout.write(
            JSON.stringify({
                GPS: {
                    latitude: parseFloat(data[1].slice(0, 2)) + parseFloat(data[1].slice(2)) / 60,
                    longitude: parseFloat(data[3].slice(0, 3)) + parseFloat(data[3].slice(3)) / 60,
                    altitude: parseFloat(data[6]),
                    speed: parseFloat(data[9]),
                }
            })
        );
    } else process.stderr.write("GPS");

    data_count++;
    if (data_count === 6) data_count = 0;

    setTimeout(getDatas);
}

// Init GPS
async function initGPS() {
    const gps_state = await execWrapper(`AT+QGPS?`);
    if (gps_state.trim().startsWith("+QGPS: 0")) {
        // Set data format and enable GPS
        await execWrapper(`AT+QGPSCFG=\"nmeasrc\",1`);
        await execWrapper(`AT+QGPS=1`);

        // Get GPS location
        await getDatas();
    } else {
        // Disable GPS and re-init
        await execWrapper(`AT+QGPSEND`);
        await initGPS();
    }
}

// Init communication with the modem
async function initCom() {
    // Disable duplicate echo
    await execWrapper(`ATE0`);

    // Init GPS
    await initGPS();
}

initCom().then();
