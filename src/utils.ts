import {exec, ExecException} from "node:child_process";
import {LogType} from "./types/log";

/**
 * Log message to the console
 * 
 * @param {string} message - Message to log
 * @param {LogType} type - Type of log message
 * @param {boolean} newLine - Add new line at the start
 * @returns {void}
 */
export function logMessage(message: string, type: LogType = LogType.INFO, newLine: boolean = false): void {
    if (process[type].isTTY) {
        process[type].write(`${newLine ? '\n' : ''}[${new Date().toLocaleTimeString('fr-FR')}] ${message}\n`);
    }
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
