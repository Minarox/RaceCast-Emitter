/**
 * @name Type
 * @description Type of component of the modem
 * @enum {string}
 */
export enum Type {
    NETWORK = 'network',
    SIGNAL = 'signal',
    GPS = 'gps'
}

/**
 * @interface Network
 * @description Network data returned by the modem
 * @enum {string}
 */
export interface Network {
    type: string;
    band: string;
    channel: number;
}

/**
 * @interface Signal
 * @description Signal data returned by the modem
 * @enum {string}
 */
export interface Signal {
    strength: number;
    error_rate: number;
}

/**
 * @interface GPS
 * @description GPS data returned by the modem
 * @enum {string}
 */
export interface GPS {
    latitude: number;
    longitude: number;
    altitude: number;
    speed: number;
}
