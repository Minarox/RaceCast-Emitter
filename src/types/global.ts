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
    stream: ChildProcess | null;
}

/**
 * Check if process argument is present
 *
 * @param {string} argument - Argument to check
 * @returns {boolean} - True if argument is present
 */
export function asProcessArg(argument: string): boolean {
    return process.argv.slice(2).includes(argument);
}
