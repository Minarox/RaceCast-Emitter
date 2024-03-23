// @ts-nocheck
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import path from 'path';
import {fileURLToPath} from 'url';
import {getToken} from './livekit';

// Variables
let datas: any = {};
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
        executablePath: 'chromium',
        args: [
            '--disable-setuid-sandbox',
            '--no-sandbox',
            '--enable-gpu',
            '--use-fake-ui-for-media-stream',
            '--autoplay-policy=no-user-gesture-required'
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
export async function launchLiveKit(): Promise<void> {
    const page = await getPage();
    await page.goto('https://live.minarox.fr', {waitUntil: 'load'});

    page.on('console', async (msg) => {
        if (process.stdout.isTTY) {
            const msgArgs = msg.args();
            for (let i = 0; i < msgArgs.length; ++i) {
                process.stdout.write(await msgArgs[i].jsonValue());
            }
        }
    });

    page.on('pageerror', async (error) => {
        if (process.stdout.isTTY) {
            process.stdout.write(`\r\nError:\r\n${error}`);
        }
    });

    await page.exposeFunction('getToken', getToken);
    await page.exposeFunction('getEnvVariable', name => getEnvVariable(name));
    await page.exposeFunction('setConnected', value => setConnected(value));
    await page.exposeFunction('getConnected', getConnected);
    const script = fs.readFileSync(`${path.dirname(fileURLToPath(import.meta.url))}/lib/livekit-client.min.js`, 'utf8');
    await page.addScriptTag({content: script});

    await page.evaluate(async () => {
        window.setConnected(false);

        async function startSession() {
            window.setConnected(false);
            let token = await window.getToken();
            let dataBuffer = false;
            let trackBuffer = false;
            let interval = null;

            let room = new LivekitClient.Room({
                adaptiveStream: false,
                dynacast: false
            });

            await room.prepareConnection(await window.getEnvVariable('API_WS_URL'), token);

            async function dataEvent(event) {
                if (dataBuffer || !window.getConnected() || !room) {
                    return;
                }
                dataBuffer = true;

                await room.localParticipant.publishData(
                    new TextEncoder().encode(JSON.stringify(event.detail.data)),
                    event.detail.lossy ? LivekitClient.DataPacket_Kind.LOSSY : LivekitClient.DataPacket_Kind.RELIABLE
                );
                dataBuffer = false;
            }

            room
                .on(LivekitClient.RoomEvent.TrackPublished, () => console.log('TrackPublished'))
                .on(LivekitClient.RoomEvent.TrackUnpublished, () => console.log('TrackUnpublished'))
                .on(LivekitClient.RoomEvent.TrackMuted, () => console.log('TrackMuted'))
                .on(LivekitClient.RoomEvent.TrackUnmuted, () => console.log('TrackUnmuted'))
                .on(LivekitClient.RoomEvent.LocalTrackPublished, () => console.log('LocalTrackPublished'))
                .on(LivekitClient.RoomEvent.LocalTrackUnpublished, () => console.log('LocalTrackUnpublished'))
                .on(LivekitClient.RoomEvent.RoomMetadataChanged, () => console.log('RoomMetadataChanged'))
                .on(LivekitClient.RoomEvent.ActiveDeviceChanged, () => console.log('ActiveDeviceChanged'))
                .on(LivekitClient.RoomEvent.MediaDevicesChanged, async () => {
                    console.log('MediaDevicesChanged');
                    await updateTracks(room);
                })
                .on(LivekitClient.RoomEvent.MediaDevicesError, () => console.log('MediaDevicesError'))
                // .on(LivekitClient.RoomEvent.ConnectionQualityChanged, () => console.log('ConnectionQualityChanged'))
                // .on(LivekitClient.RoomEvent.LocalAudioSilenceDetected, () => console.log('LocalAudioSilenceDetected'))
                .on(LivekitClient.RoomEvent.TrackStreamStateChanged, () => console.log('TrackStreamStateChanged'))
                .on(LivekitClient.RoomEvent.Connected, () => window.setConnected(true))
                .on(LivekitClient.RoomEvent.Reconnecting, () => window.setConnected(false))
                .on(LivekitClient.RoomEvent.Reconnected, () => window.setConnected(true))
                .on(LivekitClient.RoomEvent.Disconnected, async function () {
                    window.setConnected(false);

                    await room.disconnect();
                    await room.removeAllListeners();
                    window.removeEventListener('data', dataEvent);
                    clearInterval(interval);

                    room = null;
                    token = null;
                    dataBuffer = false;
                    trackBuffer = false;
                    interval = null;

                    setTimeout(startSession);
                });

            await room.connect(await window.getEnvVariable('API_WS_URL'), token);

            window.addEventListener('data', dataEvent);
            // sendToRoom();

            await updateTracks(room);

            async function updateTracks(room) {
                if (trackBuffer) {
                    return;
                }
                trackBuffer = true;

                const videoInputDevices = await LivekitClient.Room.getLocalDevices("videoinput");
                const audioInputDevices = await LivekitClient.Room.getLocalDevices("audioinput");

                const videoPublishedTrack = room.localParticipant.getTrackPublicationByName("main-video");
                const audioPublishedTrack = room.localParticipant.getTrackPublicationByName("main-audio");

                if (videoInputDevices.length) {
                    if (videoPublishedTrack) {
                        await room.switchActiveDevice("videoinput", videoInputDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || videoInputDevices[0].deviceId);
                        if (videoPublishedTrack.isMuted) {
                            videoPublishedTrack.unmute();
                        }
                    } else {
                        await room.localParticipant.publishTrack(
                            await LivekitClient.createLocalVideoTrack({
                                deviceId: videoInputDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || videoInputDevices[0].deviceId,
                                resolution: LivekitClient.VideoPresets.h360
                            }),
                            {
                                name: "main-video",
                                stream: "main",
                                source: "camera",
                                simulcast: false,
                                videoCodec: "h264",
                                videoEncoding: {
                                    maxFramerate: 25,
                                    maxBitrate: 300_000,
                                    priority: "high"
                                }
                            }
                        );
                    }
                } else if (videoPublishedTrack) {
                    await room.localParticipant.unpublishTrack(videoPublishedTrack.trackID, true);
                }

                if (audioInputDevices.length) {
                    if (audioPublishedTrack) {
                        await room.switchActiveDevice("audioinput", audioInputDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || audioInputDevices[0].deviceId);
                        if (audioPublishedTrack.isMuted) {
                            audioPublishedTrack.unmute();
                        }
                    } else {
                        await room.localParticipant.publishTrack(
                            await LivekitClient.createLocalAudioTrack({
                                deviceId: audioInputDevices.filter(device => device.label.startsWith("Cam Link 4K"))[0]?.deviceId || audioInputDevices[0].deviceId,
                                autoGainControl: false,
                                echoCancellation: false,
                                noiseSuppression: false,
                                channelCount: 2
                            }),
                            {
                                name: "main-audio",
                                stream: "main",
                                source: "audio",
                                simulcast: false,
                                red: true,
                                dtx: true,
                                stopMicTrackOnMute: true,
                                audioPreset: {
                                    maxBitrate: 48_000
                                }
                            }
                        );
                    }
                } else if (audioPublishedTrack) {
                    await room.localParticipant.unpublishTrack(audioPublishedTrack.trackID, true);
                }

                trackBuffer = false;
            }

            function sendToRoom() {
                if (window.getConnected() || room) {
                    window.saveDatas();
                }

                interval = setInterval(() => {
                    if (window.getConnected() || room) {
                        window.saveDatas();
                    }
                }, 5000)
            }
        }

        navigator.mediaDevices.enumerateDevices()
            .then(setTimeout(startSession));
    });
}

// Send data to LiveKit room
/**
 * Send data to the room
 *
 * @param {any} data - Data to send
 * @param {boolean} lossy - Request type
 * @returns {void}
 */
export function sendData(data: any, lossy: boolean = true): void {
    datas = {
        ...datas,
        ...data
    };

    if (connected && browser && page) {
        page.evaluate((data, lossy): void => {
            const customEvent: CustomEvent = new CustomEvent(
                'data',
                {
                    detail: {
                        data: data,
                        lossy: lossy
                    }
                });
            window.dispatchEvent(customEvent);
        }, data, lossy);
    }
}
