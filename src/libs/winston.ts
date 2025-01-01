import colors from "colors";
import { config, createLogger, format, transports } from "winston";

const consoleFormat = format.printf(({ level, message }): any => {
    const color: string = config.npm.colors[level] as string;
    const bgColor: string = `bg${color.slice(0, 1).toUpperCase()}${color.slice(1)}`;
    const status: string = colors.bold(colors[bgColor](` ${level.toUpperCase()} `));

    return `[${new Date().toLocaleString('fr-FR', { timeZone: 'UTC' })}] ${status} ${message}`;
});

const fileFormat = format.printf(({ level, message }): string => {
    return JSON.stringify({
        timestamp: new Date().toLocaleString('fr-FR', { timeZone: 'UTC' }),
        level: level,
        message: message
    })
})

export const logger = createLogger({
    level: process.env.LOG_LEVEL || "info",
    silent: process.env.NODE_ENV === "test",
    transports: [
        new transports.Console({
            format: consoleFormat
        }),
        new transports.File({
            dirname: "logs",
            filename: "all.log",
            format: fileFormat,
            lazy: true,
            maxsize: 1048576,
            maxFiles: 4,
            zippedArchive: true
        }),
        new transports.File({
            level: "error",
            dirname: "logs",
            filename: "errors.log",
            format: fileFormat,
            lazy: true,
            maxsize: 1048576,
            maxFiles: 4,
            zippedArchive: true
        })
    ]
});
