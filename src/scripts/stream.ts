// @ts-nocheck
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import {logMessage} from '../utils';
import {LogLevel, asProcessArg} from '../types/global';
import {getToken} from './livekit';

// Variables
let browser: any = null;
let page: any = null;
let ready: boolean = false;

/**
 * Setter to change ready value
 *
 * @param {boolean} value - New value
 * @returns {void}
 */
function setReady(value: boolean): void {
    ready = value;
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
 * Launch stream
 *
 * @returns {Promise<void>}
 */
export async function startStream(): Promise<void> {
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

    await page.goto(process.env['LIVEKIT_HTTP_URL'], {waitUntil: 'load'});

    await page.exposeFunction('getToken', getToken);
    await page.exposeFunction('setReady', value => setReady(value));
    await page.exposeFunction('asArg', argument => asProcessArg(argument));
    await page.exposeFunction('getEnvVariable', name => getEnvVariable(name));
    
    const script: string = fs.readFileSync(`${__dirname}/../libs/livekit-client.min.js`, 'utf8');
    await page.addScriptTag({content: script});

    await page.evaluate(async (): Promise<void> => {
        await window.setReady(true)

        // Variables
        let room = null;
        const fakeDevices = await asArg('fake-devices')
        const oneCamera = await asArg('one-camera');
        const tracks = {
            main: {
                video: null,
                audio: null
            },
            aux: {
                video: null,
                audio: null
            }
        };

        // Send data to room
        async function dataEvent(event) {
            if (!room) {
                return;
            }

            await room.localParticipant.publishData(
                new TextEncoder().encode(JSON.stringify(event.detail.data)),
                LivekitClient.DataPacket_Kind.LOSSY
            );
        }

        // Create local video and audio tracks
        async function createTracks() {
            const devices = await navigator.mediaDevices.enumerateDevices();
            const videoDevices = devices.filter(device => device.kind === 'videoinput');
            const audioDevices = devices.filter(device => device.kind === 'audioinput');

            const createVideoTrack = async (deviceId, maxBitrate = 400_000, priority = 'high') => {
                if (!deviceId) {
                    return null;
                }

                return await LivekitClient.createLocalVideoTrack({
                    deviceId: deviceId,
                    resolution: {
                        height: 360,
                        width: 640,
                        encoding: {
                            maxFramerate: 24,
                            maxBitrate: maxBitrate,
                            priority: priority,
                        }
                    }
                });
            }

            const createAudioTrack = async (deviceId) => {
                if (!deviceId) {
                    return null;
                }

                return await LivekitClient.createLocalAudioTrack({
                    deviceId: deviceId,
                    autoGainControl: true,
                    echoCancellation: false,
                    noiseSuppression: false,
                    channelCount: 2
                });
            }

            if (!tracks.main.video) {
                tracks.main.video = await createVideoTrack(videoDevices.find(device => device.label.startsWith('Cam Link 4K'))?.deviceId);
            }

            if (!tracks.main.audio) {
                tracks.main.audio = await createAudioTrack(audioDevices.find(device => device.label.startsWith('Cam Link 4K'))?.deviceId);
            }

            if (!oneCamera) {
                if (!tracks.aux.video) {
                    tracks.aux.video = await createVideoTrack(videoDevices.find(device => device.label.startsWith('Cam Link 4K'))?.deviceId, 300_000, 'low');
                }

                if (!tracks.aux.audio) {
                    tracks.aux.audio = await createAudioTrack(audioDevices.find(device => device.label.startsWith('Cam Link 4K'))?.deviceId);
                }
            }

            if (oneCamera && !tracks.main.video || !tracks.main.audio) {
                setTimeout(createTracks, 500);
            } else if (!tracks.main.video || !tracks.main.audio || !tracks.aux.video || !tracks.aux.audio) {
                setTimeout(createTracks, 500);
            } else {
                try {
                    console.log('Connecting to LiveKit...');
                    setTimeout(startSession);
                } catch {
                    console.log('Crash. Restarting...');
                    window.removeEventListener('data', dataEvent)
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
                    window.addEventListener('data', dataEvent);
                })
                .on(LivekitClient.RoomEvent.Reconnecting, () => {
                    console.log('Reconnecting...');
                    window.removeEventListener('data', dataEvent);
                })
                .on(LivekitClient.RoomEvent.Reconnected, () => {
                    console.log('Reconnected');
                    window.addEventListener('data', dataEvent);
                })
                .on(LivekitClient.RoomEvent.Disconnected, () => {
                    console.log('Disconnected. Restarting...');
                    window.removeEventListener('data', dataEvent);
                    room = null
                    setTimeout(startSession);
                });

            await room.connect(url, token);

            if (fakeDevices) {
                await room.localParticipant.enableCameraAndMicrophone();
            } else {
                const publishVideoTrack = async (track, stream = 'main', maxBitrate = 400_000, priority = 'high') => {
                    if (!track) {
                        return;
                    }

                    await room.localParticipant.publishTrack(track, {
                        name: `${stream}-video`,
                        stream: stream,
                        source: 'camera',
                        simulcast: false,
                        videoCodec: 'AV1',
                        videoEncoding: {
                            maxFramerate: 24,
                            maxBitrate: maxBitrate,
                            priority: priority
                        },
                        ...settings
                    });
                }

                const publishAudioTrack = async (track, stream = 'main') => {
                    if (!track) {
                        return;
                    }

                    await room.localParticipant.publishTrack(track, {
                        name: `${stream}-audio`,
                        stream: stream,
                        source: 'audio',
                        simulcast: false,
                        red: true,
                        dtx: true,
                        stopMicTrackOnMute: false,
                        audioPreset: {
                            maxBitrate: 48_000
                        }
                    });
                }

                await publishVideoTrack(tracks.main.video);
                await publishAudioTrack(tracks.main.audio);

                if (!oneCamera) {
                    await publishVideoTrack(tracks.aux.video, 'aux', 300_000, 'low');
                    await publishAudioTrack(tracks.aux.audio, 'aux');
                }                
            }
        }

        if (fakeDevices) {
            console.log('Connecting to LiveKit...');
            setTimeout(startSession);
        } else {
            console.log('Creating tracks...');
            setTimeout(createTracks);
        }
    });
}

// Send data to the room
process.on('message', (data: any): void => {
    if (browser && page && ready) {
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
