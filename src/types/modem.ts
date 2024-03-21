/**
 * Type of component of the modem
 * 
 * @enum {string}
 */
export enum Type {
    NETWORK = 'network',
    SIGNAL = 'signal',
    GPS = 'gps'
}

/**
 * Network data returned by the modem
 */
export interface Network {
    type: string;
    band: string;
    channel: number;
}

/**
 * Signal data returned by the modem
 */
export interface Signal {
    strength: number;
    error_rate: number;
}

/**
 * GPS data returned by the modem
 */
export interface GPS {
    latitude: number;
    longitude: number;
    altitude: number;
    speed: number;
}
