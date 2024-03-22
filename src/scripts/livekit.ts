import { RoomServiceClient, AccessToken } from "livekit-server-sdk";

// Variables
let token: string;
let token_created_at: number;

/**
 * Generate a token for LiveKit (valid for 6 hours)
 * 
 * @returns {Promise<string>}
 */
export async function getToken() {
    if (token && token_created_at && (Date.now() - token_created_at) < 60 * 60 * 6 * 1000) {
        return token;
    }

    // Generate a new token
    const access_token = new AccessToken(
        process.env['API_KEY'],
        process.env['API_SECRET'],
        {
            identity: "Car",
            ttl: 60 * 60 * 7,
        },
    );

    // Set permissions
    access_token.addGrant({
        roomCreate: false,
        roomJoin: true,
        roomList: false,
        roomRecord: false,
        roomAdmin: true,
        room: process.env['API_ROOM'],
        ingressAdmin: false,
        canPublish: true,
        canSubscribe: true,
        canPublishData: true,
        canUpdateOwnMetadata: false,
        hidden: false,
        recorder: false,
        agent: false
    });

    token = await access_token.toJwt();
    token_created_at = Date.now();

    return token;
}

/**
 * Update Room metadata with new data
 * 
 * @param {any} data - Data to send to the room
 * @returns {Promise<void>}
 */
export async function setRoomMetadata(data: any): Promise<void> {
    const RoomService = new RoomServiceClient(
        process.env['API_URL'],
        process.env['API_KEY'],
        process.env['API_SECRET']
    );

    await RoomService.UpdateRoomMetadata(
        process.env['API_ROOM'],
        JSON.stringify({
            ...data,
            updated_at: new Date()
        })
    );
}