import {dynamicImport} from "tsimportlib";

// Variables
let token: string;
let tokenCreatedAt: number;

/**
 * Generate a token for LiveKit (valid for 6 hours)
 *
 * @returns {Promise<string>} Token
 */
export async function getToken(): Promise<string> {
    if (token && tokenCreatedAt && (Date.now() - tokenCreatedAt) < 60 * 60 * 6 * 1000) {
        return token;
    }

    // Generate a new token
    const liveKitSDK: any = await dynamicImport('livekit-server-sdk', module) as typeof import('livekit-server-sdk');
    const accessToken: any = new liveKitSDK.AccessToken(
        process.env['API_KEY'],
        process.env['API_SECRET'],
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
        room: process.env['API_ROOM'],
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
