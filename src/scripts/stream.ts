// @ts-nocheck
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import {logMessage} from '../utils';
import {LogLevel} from '../types/global';
import {getToken} from "./livekit";

// Variables
let browser: any = null;
let page: any = null;
let connected: boolean = false;

/**
 * Setter to change connected variable value
 *
 * @param {boolean} value - New value
 * @returns {void}
 */
function setConnected(value: boolean): void {
    connected = value;
}

/**
 * Getter to fetch connected variable value
 *
 * @returns {boolean} Value of the variable
 */
function getConnected(): boolean {
    return connected;
}

/**
 * Value of the environnement variable
 *
 * @param name
 * @returns {string | undefined} Value of the variable
 */
function getEnvVariable(name: string): string | undefined {
    return process.env[name];
}

/**
 * Get process arguments
 *
 * @returns {string[]} Arguments
 */
function getProcessArgs(): string[] {
    console.log(process.argv.slice(2));
    return process.argv.slice(2);
}

/**
 * Show / Send data to the parent process
 *
 * @param {string} message - Message to send
 * @param {LogLevel} type - Type of message
 * @returns {void}
 */
function reportMessage(message: string, type: LogLevel = LogLevel.INFO): void {
    if (process.send) {
        process.send(message);
    } else {
        logMessage(message, type);
    }
}

/**
 * Launch headless browser
 *
 * @returns {Promise<any>} Instance of the browser
 */
export async function getBrowser(): Promise<any> {
    if (browser) {
        return browser;
    }

    // Launch the browser
    browser = await puppeteer.launch({
        executablePath: '/usr/bin/chromium',
        args: [
            '--disable-setuid-sandbox',
            '--no-sandbox',
            '--enable-gpu',
            '--use-fake-ui-for-media-stream',
            '--autoplay-policy=no-user-gesture-required',
            '--use-fake-device-for-media-stream'
        ],
        ignoreDefaultArgs: [
            '--mute-audio',
            '--hide-scrollbars'
        ]
    });

    // Allow permissions
    const context: any = browser.defaultBrowserContext();
    await context.overridePermissions("https://live.minarox.fr", ["microphone", "camera"]);

    return browser;
}

/**
 * Open a new page
 *
 * @returns {Promise<any>} Instance of the page
 */
async function getPage(): Promise<any> {
    if (browser && page) {
        return page;
    }

    if (!browser) {
        browser = await getBrowser();
    }

    page = await browser.newPage();
    return page;
}

/**
 * Launch stream
 *
 * @returns {Promise<void>}
 */
export async function startStream(): Promise<void> {
    const page = await getPage();
    await page.goto(process.env['LIVEKIT_HTTP_URL'], {waitUntil: 'load'});

    page.on('console', async (msg: any): Promise<void> => {
        const msgArgs = msg.args();
        for (let i = 0; i < msgArgs.length; ++i) {
            reportMessage(await msgArgs[i].jsonValue());
        }
    });

    page.on('pageerror', async (error: string): Promise<void> => {
        reportMessage(error, LogLevel.ERROR);
    });

    await page.exposeFunction('getToken', getToken);
    await page.exposeFunction('setConnected', value => setConnected(value));
    await page.exposeFunction('getConnected', getConnected);
    await page.exposeFunction('getProcessArgs', getProcessArgs);
    await page.exposeFunction('getEnvVariable', (name: string): string | undefined => getEnvVariable(name));
    const script: string = fs.readFileSync(`${__dirname}/../libs/livekit-client.min.js`, 'utf8');
    await page.addScriptTag({content: script});

    await page.evaluate(async (): Promise<void> => {
        window.setConnected(false);
        let dataListener = false;
        const tracks = {
            audio: null,
            video: null
        };

        // Create local video and audio tracks
        async function createTracks() {
            const devices = await navigator.mediaDevices.enumerateDevices();
            const audioDevices = devices.filter(device => device.kind === "audioinput");
            const videoDevices = devices.filter(device => device.kind === "videoinput");

            if (!tracks.audio) {
                const deviceId = audioDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || null;
                if (deviceId) {
                    tracks.audio = await LivekitClient.createLocalAudioTrack({
                        deviceId: deviceId,
                        autoGainControl: false,
                        echoCancellation: false,
                        noiseSuppression: false,
                        channelCount: 2
                    })
                }
            }

            if (!tracks.video) {
                const deviceId = videoDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || null;
                if (deviceId) {
                    tracks.video = await LivekitClient.createLocalVideoTrack({
                        deviceId: deviceId,
                        resolution: LivekitClient.VideoPresets.h1080.resolution
                    })
                }
            }

            if (!tracks.audio || !tracks.video) {
                setTimeout(createTracks, 500);
            } else {
                setTimeout(startSession);
            }
        }

        // Connect to LiveKit room and publish tracks with datas
        async function startSession() {
            window.setConnected(false);
            let token = await window.getToken();

            let room = new LivekitClient.Room({
                adaptiveStream: true,
                dynacast: true,
                reconnectPolicy: {
                    nextRetryDelayInMs: (context) => {
                        console.log(`Retrying connection n°${context.retryCount} (after ${context.elapsedMs}ms)`,);
                        return 200;
                    }
                }
            });

            // Send data to room
            async function dataEvent(event) {
                if (!window.getConnected() || !room) {
                    return;
                }

                await room.localParticipant.publishData(
                    new TextEncoder().encode(JSON.stringify(event.detail.data)),
                    LivekitClient.DataPacket_Kind.LOSSY
                );
            }

            await room.prepareConnection(await window.getEnvVariable('LIVEKIT_WS_URL'), token);

            room
                .on(LivekitClient.RoomEvent.Connected, () => window.setConnected(true))
                .on(LivekitClient.RoomEvent.Reconnecting, () => window.setConnected(false))
                .on(LivekitClient.RoomEvent.Reconnected, () => window.setConnected(true))
                .on(LivekitClient.RoomEvent.Disconnected, () => window.setConnected(false));

            await room.connect(await window.getEnvVariable('LIVEKIT_WS_URL'), token);

            if (window.getProcessArgs[0] === "--fake") {
                await room.localParticipant.enableCameraAndMicrophone();
            } else {
                await room.localParticipant.publishTrack(tracks.audio, {
                    name: "main-audio",
                    stream: "main",
                    source: "audio",
                    simulcast: false,
                    red: true,
                    dtx: true,
                    stopMicTrackOnMute: false,
                    audioPreset: {
                        maxBitrate: 48_000
                    }
                });

                await room.localParticipant.publishTrack(tracks.video, {
                    name: "main-video",
                    stream: "main",
                    source: "camera",
                    simulcast: false,
                    videoCodec: "AV1",
                    videoEncoding: {
                        maxFramerate: 24,
                        maxBitrate: 800_000,
                        priority: "high"
                    }
                });
            }

            if (!dataListener) {
                dataListener = true;
                window.addEventListener('data', dataEvent);
            }
        }

        if (window.getProcessArgs[0] === "--fake") {
            setTimeout(startSession);
        } else {
            setTimeout(createTracks);
        }
    });
}

// Send data to the room
process.on('message', (data: any): void => {
    if (browser && page && connected) {
        page.evaluate((data: any): void => {
            const customEvent: CustomEvent = new CustomEvent(
                'data',
                {
                    detail: {
                        data: data
                    }
                });
            window.dispatchEvent(customEvent);
        }, data);
    }
});

setTimeout(startStream);
console.log(process.argv.slice(2));
