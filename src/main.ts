// @ts-nocheck

import { $ } from "bun";
import colors from "colors";
import fs from 'fs';
import puppeteer, { Browser, BrowserContext, Page } from "puppeteer-core";
import { HTTP_URL, TLS, getLiveKitToken, updateRoomMetadata } from './libs/livekit';
import { logger } from './libs/winston';

const MODEM_ID = await $`mmcli -L | grep 'QUECTEL' | sed -n 's#.*/Modem/\([0-9]\+\).*#\1#p' | tr -d '\n'`.text();
let oldModemInfo: any = {};
let browser: Browser | null = null;
let cleanUpCalled: boolean = false;

async function cleanUp(): Promise<void> {
    if (cleanUpCalled) {
        return
    }
    cleanUpCalled = true;

    if (browser) {
        logger.verbose("Closing browser...");
        await browser.close();
    }

    logger.verbose("Exit...");
    process.exit();
}

["exit", "SIGINT", "SIGUSR1", "SIGUSR2", "uncaughtException"]
    .forEach((type: string): void => {
        process.on(type, cleanUp);
    });

export function getEnv(name: string): string {
    return process.env[name] || '';
}

async function updateEmitterInfo(): Promise<void> {
    logger.debug("Get modem info...");

    const global = await $`mmcli -m ${MODEM_ID} -J`.json();
    const location = await $`sudo mmcli -m ${MODEM_ID} --location-get -J`.json();
    let modemInfo = {};

    if (global) {
        modemInfo = {
            ...modemInfo,
            tech: global.modem?.generic['access-technologies'],
            signal: Number(global.modem?.generic['signal-quality']?.value)
        }
    }

    if (location) {
        modemInfo = {
            ...modemInfo,
            longitude: Number(location.modem?.location?.gps?.longitude?.replace(',', '.')),
            latitude: Number(location.modem?.location?.gps?.latitude?.replace(',', '.')),
            altitude: Number(location.modem?.location?.gps?.altitude?.replace(',', '.')),
            speed: Number(location.modem?.location?.gps?.nmea?.find((nmea) => nmea.startsWith('$GPVTG'))?.split(',')?.[7])
        }
    }

    if (JSON.stringify(oldModemInfo) !== JSON.stringify(modemInfo)) {
        oldModemInfo = modemInfo;
        logger.verbose("Update modem info");
        logger.debug(`New modem info: ${JSON.stringify(modemInfo)}`);
        await updateRoomMetadata(modemInfo);
    }
}

logger.debug(`TLS: ${TLS ? colors.green('enabled') : colors.red('disabled')}`);
logger.debug(`Domain: ${process.env['LIVEKIT_DOMAIN']}`);
logger.debug(`Modem ID: ${MODEM_ID}`);

await $`mmcli -m ${MODEM_ID} --enable --location-enable-gps-raw --location-enable-gps-nmea`.quiet();
setInterval(async () => await updateEmitterInfo(), 1000);

logger.debug("------------------");
logger.info('Starting browser...');

(async () => {
    browser = await puppeteer.launch({
        dumpio: process.env['LOG_LEVEL'] === 'debug',
        executablePath: "/usr/bin/chromium",
        headless: true,
        ignoreDefaultArgs: true,
        args:  [
            '--no-sandbox',
            '--disable-setuid-sandbox',
            '--headless=new',
            '--use-angle=vulkan',
            '--enable-gpu-rasterization',
            '--use-vulkan',
            '--enable-gpu',
            '--disable-vulkan-surface',
            '--enable-unsafe-webgpu',
            '--disable-search-engine-choice-screen',
            '--ash-no-nudges',
            '--no-first-run',
            '--disable-features=Translate',
            '--no-default-browser-check',
            '--allow-chrome-scheme-url',
            '--use-fake-ui-for-media-stream',
            '--autoplay-policy=no-user-gesture-required',
            '--ignore-gpu-blocklist'
        ]
    });
    const context: BrowserContext = browser.defaultBrowserContext();
    await context.overridePermissions(HTTP_URL, ['microphone', 'camera']);

    logger.info('Opening new page...');
    const page: Page = await browser.newPage();

    logger.info(`Loading ${HTTP_URL}...`);
    await page.goto(HTTP_URL);
    await page.addScriptTag({ content: fs.readFileSync(`${__dirname}/libs/livekit-client.umd.min.js`, 'utf8') });
    await page.exposeFunction('getLiveKitToken', getLiveKitToken);
    await page.exposeFunction('getEnv', getEnv);
    await page.exposeFunction('logWarn', (message: string) => { logger.warn(message) });
    await page.exposeFunction('logInfo', (message: string) => { logger.info(message) });
    await page.exposeFunction('logDebug', (message: string) => { logger.debug(message) });

    page.on('pageerror', error => {
        logger.error(error.message);
    });

    page.on('requestfailed', request => {
        logger.error(`Request Failed: ${request.failure()?.errorText}, ${request.url()}`);
    });

    page.on('console', async (msg: any): Promise<void> => {
        const msgArgs = msg.args();
        for (let i = 0; i < msgArgs.length; ++i) {
            logger.debug(JSON.stringify(await msgArgs[i].jsonValue()));
        }
    });

    await page.evaluate(async (): Promise<void> => {
        await window.logInfo("Starting LiveKit...");

        const TLS = await window.getEnv('LIVEKIT_TLS') === 'true';
        const WS_URL = `ws${TLS ? 's' : ''}://${await window.getEnv('LIVEKIT_DOMAIN')}`;
        const TOKEN = await window.getLiveKitToken();
        let devicesBuffer = [];

        const room = new LivekitClient.Room({
            reconnectPolicy: {
                nextRetryDelayInMs: () => {
                    return 1000;
                }
            }
        });

        await window.logInfo("Connecting to LiveKit server...");
        await room.prepareConnection(WS_URL, TOKEN);

        // Create and publish tracks
        const createTracks = async (devices = []) => {
            devices.forEach(async (device) => {
                const publishTrackOptions = {
                    name: device.label,
                    stream: device.groupId,
                    simulcast: false
                }

                if (device.kind === "videoinput") {
                    await window.logDebug(`Add video track: ${device.label}`);
                    await room.localParticipant.publishTrack(
                        await LivekitClient.createLocalVideoTrack({
                            deviceId: device.deviceId
                        }),
                        {
                            ...publishTrackOptions,
                            source: LivekitClient.Track.Source.Camera,
                            degradationPreference: "maintain-resolution",
                            videoCodec: "AV1",
                            /* videoEncoding: {
                                maxFramerate: 30,
                                maxBitrate: 2_500_000,
                            } */
                        }
                    );
                } else if (device.kind === "audioinput") {
                    await window.logDebug(`Add audio track: ${device.label}`);
                    await room.localParticipant.publishTrack(
                        await LivekitClient.createLocalAudioTrack({
                            deviceId: device.deviceId,
                            autoGainControl: false,
                            echoCancellation: false,
                            noiseSuppression: false
                        }),
                        {
                            ...publishTrackOptions,
                            source: LivekitClient.Track.Source.Microphone,
                            red: true,
                            dtx: true,
                            stopMicTrackOnMute: false,
                            /* audioPreset: {
                                maxBitrate: 36_000
                            } */
                        }
                    );
                }
            });
        }

        // Unpublish and remove tracks
        const removeTracks = async (devices = []) => {
            devices.forEach(async (device) => {
                const trackPublication = room.localParticipant.getTrackPublicationByName(device.label);
                if (trackPublication?.track) {
                    await window.logDebug("Remove track: ", device.label);
                    await room.localParticipant.unpublishTrack(trackPublication.track);
                }
            });
        }

        const checkTracks = async () => {
            const devices = (await navigator.mediaDevices.enumerateDevices())
                .filter(device =>
                    ['audioinput', 'videoinput'].includes(device.kind) &&
                    !["Default", "HD USB Camera  HD USB Camera", "Cam Link 4K  Cam Link 4K"].includes(device.label)
                );

            const addedDevices = devices.filter(currentDevice =>
                !devicesBuffer.some(previousDevice => previousDevice.deviceId === currentDevice.deviceId)
            );
            const removedDevices = devicesBuffer.filter(previousDevice =>
                !devices.some(currentDevice => currentDevice.deviceId === previousDevice.deviceId)
            );

            await removeTracks(removedDevices);
            await createTracks(addedDevices);
            devicesBuffer = devices;
        }

        room
            .on(LivekitClient.RoomEvent.Connected, async () => {
                await window.logInfo("Connected");
            })
            .on(LivekitClient.RoomEvent.Reconnecting, async () => {
                await window.logWarn("Reconnecting...");
            })
            .on(LivekitClient.RoomEvent.Reconnected, async () => {
                await window.logInfo("Reconnected");
            })
            .on(LivekitClient.RoomEvent.Disconnected, async () => {
                await window.logWarn("Disconnected");
            })
            .on(LivekitClient.RoomEvent.MediaDevicesChanged, async () => {
                await window.logDebug("Media devices changed");
                await checkTracks();
            })
            .on(LivekitClient.RoomEvent.MediaDevicesError, async () => {
                await window.logDebug("Media devices error");
                await checkTracks();
            });

        await room.connect(WS_URL, TOKEN);
        await checkTracks();
    });
})();
