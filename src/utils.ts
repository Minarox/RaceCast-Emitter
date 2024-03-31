import {exec, ExecException} from "node:child_process";
import {LogLevel} from "./types/global";

/**
 * Log message to the console
 *
 * @param {string} message - Message to log
 * @param {LogLevel} level - Level of log message
 * @returns {void}
 */
export function logMessage(message: string, level: LogLevel = LogLevel.INFO): void {
    const type: string = level === LogLevel.ERROR ? 'stderr' : 'stdout';

    // if (process[type].isTTY) {
    process[type].write(`[${new Date().toLocaleTimeString('fr-FR')}] [${level}] ${message}\n`);
    // }
}

/**
 * Execute command on the host
 *
 * @param {string} command - Command to execute
 * @returns {Promise<string>} Result of the command
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
 * Execute AT command and return response
 *
 * @param {string} command - AT command to execute
 * @returns {Promise<string>} Modem response
 */
export function executeAT(command: string): Promise<string> {
    return execute(`echo '${command}' | socat - /dev/ttyUSB2,crnl`);
}

/**
 * Wait a certain amount of time
 *
 * @params {number} ms - Time to wait in millisecond
 * @returns {void}
 */
export function wait(ms: number): void {
    const start: number = Date.now();
    let now: number = start;

    while (now - start < ms) {
        now = Date.now();
    }
}
