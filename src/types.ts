import {ChildProcess} from 'child_process';

/**
 * Level of log message
 *
 * @enum {string}
 */
export enum LogLevel {
    INFO = 'INFO',
    WARNING = 'WARNING',
    ERROR = 'ERROR',
    DATA = 'DATA'
}

/**
 * Processes used by the program
 */
export interface Processes {
    gps: ChildProcess | null;
    livekit: ChildProcess | null;
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
