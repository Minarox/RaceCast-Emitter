import {exec, ExecException} from "node:child_process";
import {LogLevel} from "./types";
import {dynamicImport} from "tsimportlib";

// Variables
let liveKitSDK: any;
let token: string;
let tokenCreatedAt: number;
let updatingMetadata: boolean = false;

/**
 * @description Log message to the console
 * @param {string} message - Message to log
 * @param {LogLevel} level - Level of log message
 * @returns {void}
 */
export function logMessage(message: string, level: LogLevel = LogLevel.INFO): void {
    const type: string = level === LogLevel.ERROR ? 'stderr' : 'stdout';

    process[type].write(`[${new Date().toLocaleTimeString('fr-FR')}] [${level}] ${message}\n`);
}

/**
 * @description Execute command on the host
 * @param {string} command - Command to execute
 * @returns {Promise<string>} Result of the command
 */
export function execute(command: string): Promise<string> {
    return new Promise((resolve, reject): void => {
        exec(command, (error: ExecException | null, stdout: string, stderr: string): void => {
            if (error || stderr) {
                reject(error || stderr);
            }
            resolve(stdout);
        });
    });
}

/**
 * @description Execute AT command and return response
 * @param {string} command - AT command to execute
 * @returns {Promise<string>} Modem response
 */
export function executeAT(command: string): Promise<string> {
    return execute(`echo '${command}' | socat - /dev/ttyUSB2,crnl`);
}

/**
 * @description Wait a certain amount of time
 * @params {number} ms - Time to wait in millisecond
 * @returns {void}
 */
export function wait(ms: number): void {
    const start: number = Date.now();
    let now: number = start;

    while (now - start < ms) {
        now = Date.now();
    }
}

/**
 * @description Value of the environnement variable
 * @param name
 * @returns {string | undefined} Value of the variable
 */
export function getEnvVariable(name: string): string | undefined {
    return process.env[name];
}

/**
 * @description Check if process argument is present
 * @param {string} argument - Argument to check
 * @returns {boolean} - True if argument is present
 */
export function asProcessArg(argument: string): boolean {
    return process.argv.slice(2).includes(argument);
}

/**
 * @description Return a random integer between min (included) and max (included)
 * @param {number} min - Minimum value
 * @param {number} max - Maximum value
 * @param {boolean} integer - Return an integer
 * @returns {number} Random integer
 */
export function getRandomInt(min, max) {
    min = Math.ceil(min);
    max = Math.floor(max);
    return Math.floor(Math.random() * (max - min + 1)) + min;
}

/**
 * @description Return a random float between min (included) and max (included)
 * @param {number} min - Minimum value
 * @param {number} max - Maximum value
 * @param {number} precision - Precision of the float
 * @returns {number} Random float
 */
export function getRandomFloat(min, max, precision = 2) {
    return parseFloat((Math.random() * (max - min) + min).toFixed(precision));
}

/**
 * @description Load LiveKit SDK
 * @returns {Promise<void>}
 */
async function loadSDK(): Promise<void> {
    if (!liveKitSDK) {
        liveKitSDK = await dynamicImport('livekit-server-sdk', module) as typeof import('livekit-server-sdk');
    }
}

/**
 * @description Generate a token for LiveKit (valid for 6 hours)
 * @returns {Promise<string>} Token
 */
export async function getToken(): Promise<string> {
    if (token && tokenCreatedAt && (Date.now() - tokenCreatedAt) < 60 * 60 * 6 * 1000) {
        return token;
    }

    // Load SDK
    await loadSDK();

    // Generate a new token
    const accessToken: any = new liveKitSDK.AccessToken(
        process.env['LIVEKIT_KEY'],
        process.env['LIVEKIT_SECRET'],
        {
            identity: "Car",
            ttl: 60 * 60 * 7,
        },
    );

    // Set permissions
    accessToken.addGrant({
        roomCreate: false,
        roomJoin: true,
        roomList: false,
        roomRecord: false,
        roomAdmin: false,
        room: process.env['LIVEKIT_ROOM'],
        ingressAdmin: false,
        canPublish: true,
        canSubscribe: true,
        canPublishData: true,
        canUpdateOwnMetadata: true,
        hidden: false,
        recorder: false,
        agent: false
    });

    token = await accessToken.toJwt();
    tokenCreatedAt = Date.now();

    return token;
}

/**
 * @description Update room metadata
 * @param {any} metadata - Metadata
 * @returns {Promise<void>}
 */
export async function updateRoomMetadata(metadata: any): Promise<void> {
    if (updatingMetadata) {
        return;
    }
    updatingMetadata = true;

    // Load SDK
    await loadSDK();

    // Create a new RoomService
    const roomService = new liveKitSDK.RoomServiceClient(
        process.env['LIVEKIT_HTTP_URL'],
        process.env['LIVEKIT_KEY'],
        process.env['LIVEKIT_SECRET']
    );

    // Fetch room
    const room: any = (await roomService.listRooms())
        .find((room: any): boolean => room.name === process.env['LIVEKIT_ROOM']);
    const roomMetadata = room.metadata ? JSON.parse(room.metadata) : {};
    metadata = { ...metadata, last_update: Date.now() };

    // Update room metadata
    await roomService.updateRoomMetadata(
        process.env['LIVEKIT_ROOM'],
        JSON.stringify({
            ...roomMetadata,
            car: {
                ...metadata
            }
        })
    );

    updatingMetadata = false;
}
