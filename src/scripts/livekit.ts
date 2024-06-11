// @ts-nocheck
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import {logMessage, getEnvVariable, asProcessArg, getToken, updateRoomMetadata} from '../utils';
import {LogLevel} from '../types';

// Variables
let browser: any = null;
let page: any = null;

/**
 * @description Show / Send data to the parent process
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
 * @description Launch headless browser
 * @returns {Promise<any>} Instance of the browser
 */
export async function newPage(): Promise<any> {
    if (browser) {
        if (!page) {
            page = await browser.newPage();
        }
        return page;
    }

    // Prepare args
    const args: string[] = [
        '--disable-setuid-sandbox',
        '--no-sandbox',
        '--enable-gpu',
        '--use-fake-ui-for-media-stream',
        '--autoplay-policy=no-user-gesture-required'
    ]
    if (asProcessArg('fake-devices')) {
        args.push('--use-fake-device-for-media-stream')
    }

    // Launch the browser
    browser = await puppeteer.launch({
        executablePath: '/usr/bin/chromium',
        args: args,
        ignoreDefaultArgs: [
            '--mute-audio',
            '--hide-scrollbars'
        ]
    });

    // Allow permissions
    const context: any = browser.defaultBrowserContext();
    await context.overridePermissions(process.env['LIVEKIT_HTTP_URL'], ['microphone', 'camera']);

    // Open new page
    page = await browser.newPage();
    return page;
}

/**
 * @description Start the broadcast
 * @returns {Promise<void>}
 */
export async function startBroadcast(): Promise<void> {
    const page = await newPage();

    page.on('console', async (msg: any): Promise<void> => {
        const msgArgs = msg.args();
        for (let i = 0; i < msgArgs.length; ++i) {
            reportMessage(await msgArgs[i].jsonValue());
        }
    });

    page.on('pageerror', async (error: string): Promise<void> => {
        reportMessage(error, LogLevel.ERROR);
    });

    await page.goto(process.env['LIVEKIT_HTTP_URL'], { waitUntil: 'load' });

    await page.exposeFunction('getToken', getToken);
    await page.exposeFunction('asProcessArg', value => asProcessArg(value));
    await page.exposeFunction('getEnvVariable', name => getEnvVariable(name));
    await page.exposeFunction('sendData', data => updateRoomMetadata(data));

    const script: string = fs.readFileSync(`${__dirname}/../libs/livekit-client.min.js`, 'utf8');
    await page.addScriptTag({ content: script });

    await page.evaluate(async (): Promise<void> => {
        // Variables
        let room = null;
        let devicesBuffer = [];
        const fakeDevices = await window.asProcessArg('fake-devices');

        // Connect to LiveKit room and publish tracks
        async function startLiveKit() {
            const token = await window.getToken();
            const url = await window.getEnvVariable('LIVEKIT_WS_URL');

            room = new LivekitClient.Room({
                adaptiveStream: true,
                dynacast: true,
                reconnectPolicy: {
                    nextRetryDelayInMs: (context) => {
                        return 600;
                    }
                }
            });

            await room.prepareConnection(url, token);

            // Create and publish tracks
            const createTracks = async (devices = []) => {
                devices.forEach(async (device) => {
                    const publishTrackOptions = {
                        name: device.label,
                        stream: device.groupId,
                        simulcast: false
                    }

                    if (device.kind === 'videoinput') {
                        await room.localParticipant.publishTrack(
                            await LivekitClient.createLocalVideoTrack({
                                deviceId: device.deviceId
                            }),
                            {
                                ...publishTrackOptions,
                                source: LivekitClient.Track.Source.Camera,
                                degradationPreference: 'maintain-framerate',
                                videoEncoding: {
                                    maxFramerate: 30,
                                    maxBitrate: 1_500_000,
                                }
                            }
                        );
                    } else if (device.kind === 'audioinput') {
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
                                audioPreset: {
                                    maxBitrate: 40_000
                                }
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
                        await room.localParticipant.unpublishTrack(trackPublication.track);
                    }
                });
            }

            const checkTracks = async () => {
                const devices = (await navigator.mediaDevices.enumerateDevices())
                    .filter(device =>
                        ['audioinput', 'videoinput'].includes(device.kind) &&
                        (fakeDevices || device.deviceId !== 'default' || device.label !== 'Default')
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
                    console.log('Connected');
                    sendData({});
                    await checkTracks();
                })
                .on(LivekitClient.RoomEvent.Reconnecting, () => {
                    console.log('Reconnecting...');
                })
                .on(LivekitClient.RoomEvent.Reconnected, () => {
                    console.log('Reconnected');
                    sendData({});
                })
                .on(LivekitClient.RoomEvent.Disconnected, async () => {
                    console.log('Disconnected. Restarting...');
                    await removeTracks(devicesBuffer);
                    devicesBuffer = [];
                    room = null;
                    setTimeout(startLiveKit);
                })
                .on(LivekitClient.RoomEvent.MediaDevicesChanged, async () => {
                    console.log('Media devices changed');
                    await checkTracks();
                })
                .on(LivekitClient.RoomEvent.MediaDevicesError, async () => {
                    console.log('Media devices error');
                    await checkTracks();
                });

            await room.connect(url, token);
        }

        console.log('Starting LiveKit...');
        try {
            setTimeout(startLiveKit);
        } catch {
            console.log('Crash. Restarting LiveKit...');
            devicesBuffer = [];
            room = null;
            setTimeout(startLiveKit);
        }
    });
}

try {
    setTimeout(startBroadcast);
} catch {
    reportMessage('An error occurred while starting the broadcast.', LogLevel.ERROR);
    browser.close().then((): void => {
        process.exit(1);
    });
}
