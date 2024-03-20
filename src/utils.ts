import {exec, ExecException} from "node:child_process";

/**
 * @name LogType
 * @description Type of log message
 * @enum {string}
 */
export enum LogType {
    INFO = 'stdout',
    ERROR = 'stderr'
}

/**
 * @function logMessage
 * @description Log message to the console
 * @param {string} message - message to log
 * @param {LogType} type - type of log message
 * @param {boolean} exiting - is the script exiting
 * @returns {void}
 */
export function logMessage(message: string, type: LogType = LogType.INFO, exiting: boolean = false): void {
    if (process[type].isTTY) {
        process[type].write(`${exiting ? '\n' : ''}[${new Date().toLocaleTimeString('fr-FR')}] ${message}\n`);
    }
}

/**
 * @function execute
 * @description Execute command on the host
 * @param {string} command - Command to execute
 * @returns {Promise<string>} result of the command
 */
export function execute(command: string): Promise<string> {
    return new Promise((resolve, reject): void => {
        exec(command, (error: ExecException | null, stdout: string, stderr: string): void => {
            if (error || stderr) {
                reject(error || stderr);
            }
            resolve(stdout);
        });
    });
}

/**
 * @function executeAT
 * @description Execute AT command and return response
 * @param {string} command - AT command to execute
 * @returns {Promise<string>} modem response
 */
export function executeAT(command: string): Promise<string> {
    return execute(`echo '${command}' | socat - /dev/ttyUSB2,crnl`);
}
