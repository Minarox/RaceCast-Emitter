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
 * GPS data returned by the modem
 */
export interface GPS {
    latitude: number;
    longitude: number;
    altitude: number;
    speed: number;
}
