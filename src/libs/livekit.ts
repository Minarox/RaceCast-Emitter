import * as LiveKit from 'livekit-server-sdk';
import { logger } from './winston.ts';

export const TLS = process.env.LIVEKIT_TLS === 'true';
export const HTTP_URL = `http${TLS ? 's' : ''}://${process.env.LIVEKIT_DOMAIN}`;

let token: string;
let tokenCreatedAt: number;

export async function getLiveKitToken(): Promise<string> {
    if (token && tokenCreatedAt && (Date.now() - tokenCreatedAt) < 60 * 60 * 12 * 1000) {
        return token;
    }

    // Generate a new token
    const accessToken: LiveKit.AccessToken = new LiveKit.AccessToken(
        process.env.LIVEKIT_KEY,
        process.env.LIVEKIT_SECRET,
        {
            identity: "car",
            ttl: 60 * 60 * 13,
        },
    );

    // Set permissions
    accessToken.addGrant({
        roomCreate: false,
        roomJoin: true,
        roomList: false,
        roomRecord: false,
        roomAdmin: false,
        room: process.env.LIVEKIT_ROOM,
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

export async function updateRoomMetadata(metadata: any): Promise<void> {
    // Create a new RoomService
    const roomService: LiveKit.RoomServiceClient = new LiveKit.RoomServiceClient(
        HTTP_URL,
        process.env.LIVEKIT_KEY,
        process.env.LIVEKIT_SECRET
    );

    // Fetch room
    try {
        const rooms: any = await roomService.listRooms();
        const room: any = rooms.find((room: any): boolean => room.name === process.env.LIVEKIT_ROOM);

        if (room) {
            const roomMetadata: any = room?.metadata ? JSON.parse(room.metadata) : {};

            // Update room metadata
            await roomService.updateRoomMetadata(
                process.env.LIVEKIT_ROOM as string,
                JSON.stringify({
                    ...roomMetadata,
                    car: {
                        ...metadata,
                        last_update: Date.now()
                    }
                })
            )
        }
    } catch (error) {
        logger.error(error)
    }
}
