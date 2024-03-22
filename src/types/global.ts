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
    modem: ChildProcess | null;
    sensor: ChildProcess | null;
}
