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

    // Import SDK
    const liveKitSDK: any = await dynamicImport('livekit-server-sdk', module) as typeof import('livekit-server-sdk');

    // Check room
    const roomService = new liveKitSDK.RoomServiceClient(
        process.env['LIVEKIT_HTTP_URL'],
        process.env['LIVEKIT_KEY'],
        process.env['LIVEKIT_SECRET']
    );

    const rooms: any = await roomService.listRooms();
    const createRoom: boolean = rooms.some((room: any): boolean => {
        if (room.name === process.env['LIVEKIT_ROOM']) {
            return true;
        }
        return false;
    })
    
    if (createRoom) {
          await roomService.createRoom({
            departureTimeout: 999999999,
            emptyTimeout: 999999999,
            name: process.env['LIVEKIT_ROOM']
          });
    }

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
