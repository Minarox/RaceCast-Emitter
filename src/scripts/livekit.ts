// @ts-nocheck
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import {logMessage, getEnvVariable, asProcessArg, getToken, updateRoomMetadata} from '../utils';
import {LogLevel} from '../types';

// Variables
let browser: any = null;
let page: any = null;

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
 * Start the broadcast
 *
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
        const defaultDevices = await window.asProcessArg('default-devices');
        const fakeDevices = await window.asProcessArg('fake-devices');
        const noCamera = await window.asProcessArg('no-camera');
        const tracks = {
            video: null,
            audio: null
        };

        // Create local video and audio tracks
        async function createTracks() {
            const devices = await navigator.mediaDevices.enumerateDevices();

            if (!noCamera && !tracks.video) {
                const deviceId = devices
                    .filter(device => device.kind === 'videoinput')
                    .find(device => device.label.startsWith('Cam Link 4K'))
                    ?.deviceId;

                if (deviceId) {
                    tracks.video = await LivekitClient.createLocalVideoTrack({
                        deviceId: deviceId,
                        resolution: {
                            width: 1920,
                            height: 780,
                            encoding: {
                                maxFramerate: 30,
                                maxBitrate: 6_000_000,
                                priority: 'high',
                            }
                        }
                    });
                }
            }

            if (!tracks.audio) {
                const deviceId = devices
                    .filter(device => device.kind === 'audioinput')
                    .find(device => device.label.startsWith('Cam Link 4K'))
                    ?.deviceId;

                if (deviceId) {
                    tracks.audio = await LivekitClient.createLocalAudioTrack({
                        deviceId: deviceId,
                        autoGainControl: false,
                        echoCancellation: false,
                        noiseSuppression: false,
                        channelCount: 1
                    });
                }
            }

            if (!tracks.video || !tracks.audio) {
                setTimeout(createTracks, 200);
            } else {
                try {
                    console.log('Connecting to LiveKit...');
                    setTimeout(startSession);
                } catch {
                    console.log('Crash. Restarting...');
                    room = null
                    setTimeout(startSession);
                }
            }
        }

        // Connect to LiveKit room and publish tracks with datas
        async function startSession() {
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

            room
                .on(LivekitClient.RoomEvent.Connected, () => {
                    console.log('Connected');
                    sendData({});
                })
                .on(LivekitClient.RoomEvent.Reconnecting, () => {
                    console.log('Reconnecting...');
                })
                .on(LivekitClient.RoomEvent.Reconnected, () => {
                    console.log('Reconnected');
                    sendData({});
                })
                .on(LivekitClient.RoomEvent.Disconnected, () => {
                    console.log('Disconnected. Restarting...');
                    room = null
                    setTimeout(startSession);
                });

            await room.connect(url, token);

            if (fakeDevices || defaultDevices) {
                const devices = await navigator.mediaDevices.enumerateDevices();

                if (devices.filter(device => device.kind === 'videoinput').length > 0 && !noCamera) {
                    await room.localParticipant.setCameraEnabled(true);
                }

                if (devices.filter(device => device.kind === 'audioinput').length > 0) {
                    await room.localParticipant.setMicrophoneEnabled(true);
                }
            } else {
                if (tracks.video && !noCamera) {
                    await room.localParticipant.publishTrack(tracks.video, {
                        name: 'main-video',
                        stream: 'main',
                        source: 'camera',
                        simulcast: false,
                        videoCodec: 'AV1',
                        videoEncoding: {
                            maxFramerate: 24,
                            maxBitrate: 400_000,
                            priority: 'high'
                        }
                    });
                }

                if (tracks.audio) {
                    await room.localParticipant.publishTrack(tracks.audio, {
                        name: 'main-audio',
                        stream: 'main',
                        source: 'audio',
                        simulcast: false,
                        red: true,
                        dtx: true,
                        stopMicTrackOnMute: false,
                        audioPreset: {
                            maxBitrate: 16_000
                        }
                    });
                }
            }
        }

        if (fakeDevices || defaultDevices) {
            console.log('Connecting to LiveKit...');
            try {
                setTimeout(startSession);
            } catch {
                console.log('Crash. Restarting...');
                room = null
                setTimeout(startSession);
            }
        } else {
            console.log('Creating tracks...');
            setTimeout(createTracks);
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
