import type { getLiveKitToken } from "./libs/livekit.ts";
import type { logger } from "./libs/winston.ts";
import type { getEnv } from "./main.ts";

declare global {
    namespace NodeJS {
        interface ProcessEnv {
            LIVEKIT_TLS: string;
            LIVEKIT_DOMAIN: string;
            LIVEKIT_KEY: string;
            LIVEKIT_SECRET: string;
            LIVEKIT_ROOM: string;
            LOG_LEVEL: 'verbose' | 'debug' | 'info' | 'warn' | 'error';
            CHROME_PATH: string;
        }
    }

    interface Window {
        getLiveKitToken: typeof getLiveKitToken;
        getEnv: typeof getEnv;
        logWarn: typeof logger.warn;
        logInfo: typeof logger.info;
        logDebug: typeof logger.debug;
    }
}
