import { execSync, spawn, type ChildProcessWithoutNullStreams } from "child_process";
import colors from "colors";
import { TLS, updateRoomMetadata } from './libs/livekit.ts';
import { logger } from './libs/winston.ts';
import { startBrowser, closeBrowser } from './libs/puppeteer.ts';

const MODEM_ID: number = Number(execSync(`mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\x01#p' | tr -d '\n'`));
let oldModemInfo: any = {};
let mpuProcess: ChildProcessWithoutNullStreams | null = null;
let temperature: number | null = null;
let cleanUpCalled: boolean = false;

/**
 * Clean up function to handle process exit and errors.
 *
 * @param {any} error - The error object if an error occurred.
 * @returns {Promise<void>} - A promise that resolves when the cleanup is complete.
 */
async function cleanUp(error: any): Promise<void> {
    if (cleanUpCalled) {
        return
    }
    cleanUpCalled = true;

    if (error?.toString()?.split(':')?.[0]?.includes('Error')) {
        logger.error(error.toString());
    }

    // Kill MPU script if it's running
    if (mpuProcess !== null) {
        logger.verbose("Kill MPU script...");
        mpuProcess.kill();
    }

    // Close the browser
    await closeBrowser();

    logger.verbose("Exit...");
    process.exit();
}

["exit", "SIGINT", "SIGQUIT", "SIGTERM", "SIGUSR1", "SIGUSR2", "uncaughtException", "SIGUSR2"]
    .forEach((type: string): void => {
        process.on(type, cleanUp);
    });

/**
 * Parses a value to a number.
 *
 * @param {unknown} value - The value to parse.
 * @returns {number | null} - Returns the parsed number or null if the value is not a valid number.
 */
function parseNumber(value: unknown): number | null {
    const result = Number(value ?? undefined)
    return isNaN(result) ? null : result
}

/**
 * Updates LiveKit room metadata with the latest data (Modem, GPS, MPU).
 *
 * @returns {Promise<void>} - A promise that resolves when the update is complete.
 */
async function updateEmitterInfo(): Promise<void> {
    logger.debug("Get modem info...");

    const global = JSON.parse(execSync(`sudo mmcli -m ${MODEM_ID} -J`).toString() || '{}')?.modem?.generic;
    const location = JSON.parse(execSync(`sudo mmcli -m ${MODEM_ID} --location-get -J`).toString() || '{}')?.modem?.location?.gps;
    const modemInfo = {
        tech: global?.['access-technologies'],
        signal: parseNumber(global?.['signal-quality']?.value),
        longitude: parseNumber(location?.longitude?.replace(',', '.')),
        latitude: parseNumber(location?.latitude?.replace(',', '.')),
        altitude: parseNumber(location?.altitude?.replace(',', '.')),
        speed: parseNumber(location?.nmea?.find((nmea: string) => nmea?.startsWith('$GPVTG'))?.split(',')?.[7] || null),
        temperature: temperature
    };

    if (oldModemInfo !== JSON.stringify(modemInfo)) {
        oldModemInfo = JSON.stringify(modemInfo);
        logger.verbose("Update modem info");
        logger.debug(`New modem info: ${JSON.stringify(modemInfo)}`);
        await updateRoomMetadata(modemInfo);
    }
}

/**
 * Starts the MPU script to read temperature data.
 *
 * @returns {void}
 */
function startMPU(): void {
    if (mpuProcess !== null) {
        return;
    }
    logger.verbose("Start MPU script...");

    mpuProcess = spawn("node", ["src/libs/mpu.ts"], { stdio: "pipe" });

    mpuProcess.stdout.on('data', (data: any) => {
        temperature = parseNumber(data.toString().trim());
        logger.debug(`New sensor info: ${temperature}`);
    });

    ["exit", "error"].forEach((type) => {
        mpuProcess?.on(type, () => {
            if (!cleanUpCalled) {
                logger.error(`MPU script exited with code ${mpuProcess?.exitCode}.`);
            }

            mpuProcess?.kill();
            mpuProcess = null;

            if (!cleanUpCalled) {
                setTimeout(startMPU, 1000);
            }
        });
    });
}

// ---------------------------------------------------

logger.debug(`TLS: ${TLS ? colors.green('enabled') : colors.red('disabled')}`);
logger.debug(`Domain: ${process.env.LIVEKIT_DOMAIN}`);
logger.debug(`Modem ID: ${MODEM_ID}`);

// startMPU();

// try {
//     logger.debug('Enable GPS location...');
//     execSync(`sudo mmcli -m ${MODEM_ID} --enable --location-enable-gps-raw --location-enable-gps-nmea`);
//     setInterval(async () => await updateEmitterInfo(), 1000);
// } catch (error: any) {
//     logger.error(error);
// }

logger.debug("------------------");
startBrowser();