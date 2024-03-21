import { ChildProcess } from 'child_process';

/**
 * Type of log message
 * 
 * @enum {string}
 */
export enum LogType {
    INFO = 'stdout',
    ERROR = 'stderr'
}

/**
 * Processes used by the program
 */
export interface Processes {
    modem: ChildProcess | null;
    sensor: ChildProcess | null;
}