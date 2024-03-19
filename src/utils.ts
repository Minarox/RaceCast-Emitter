/**
 * @description Type of log message
 */
export enum LogType {
    INFO = 'stdout',
    ERROR = 'stderr'
}

/**
 * @description Log message to the console
 * @param {string} message - message to log
 * @param {LogType} type - type of log message
 * @param {boolean} exiting - is the script exiting
 * @return {void}
 */
export function logMessage(message: string, type: LogType = LogType.INFO, exiting: boolean = false): void {
    if (process[type].isTTY) {
        process[type].write(`${exiting ? '\n' : ''}[${new Date().toLocaleTimeString('fr-FR')}] ${message}\n`);
    }
}
