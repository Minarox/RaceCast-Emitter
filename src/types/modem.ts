export enum Type {
    NETWORK = 'network',
    SIGNAL = 'signal',
    GPS = 'gps'
}

export interface Network {
    access: string;
    band: string;
    channel: number;
}

export interface Signal {
    signal: number;
    error_rate: number;
}

export interface GPS {
    latitude: number;
    longitude: number;
    altitude: number;
    speed: number;
}
