// @ts-nocheck

import { fileURLToPath } from "url";
import { dirname } from "path";
import puppeteer, { Browser, BrowserContext, Page } from "puppeteer-core";
import fs from 'fs';
import { HTTP_URL, getLiveKitToken } from './livekit.ts';
import { logger } from './winston.ts';

const __filename: string = fileURLToPath(import.meta.url);
const __dirname: string = dirname(__filename);
let browser: Browser | null = null;

/**
 * Closes the Puppeteer browser instance if it is open.
 *
 * @returns {Promise<void>} - A promise that resolves when the browser is closed.
 */
export async function closeBrowser(): Promise<void> {
    if (browser) {
        logger.verbose("Closing browser...");
        await browser.close();
        browser = null;
    }
}

/**
 * Starts the Puppeteer browser and sets up the LiveKit client.
 *
 * @returns {Promise<void>} - A promise that resolves when the browser is started and the page is set up.
 */
export const startBrowser = async (): Promise<void> => {
    logger.info('Starting browser...');

    browser = await puppeteer.launch({
        dumpio: process.env.LOG_LEVEL === 'debug',
        executablePath: process.env.CHROME_PATH,
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
            '--disable-features=WebRtcPipeWireCamera',
            '--enable-features=VaapiVideoEncoder,VaapiVideoDecodeLinuxGL',
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
    await page.addScriptTag({ content: fs.readFileSync(`${__dirname}/livekit-client.umd.min.js`, 'utf8') });
    await page.exposeFunction('getLiveKitToken', getLiveKitToken);
    await page.exposeFunction('getEnv', (envKey: string): string => { return process.env[envKey] || '' });
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
                            degradationPreference: "maintain-framerate",
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
                            red: false,
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
                    (device.kind === 'audioinput' && device.label.endsWith('Analog Stereo')) ||
                    (device.kind === 'videoinput')
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
}
