import i2c, {I2CBus} from "i2c-bus";
import Mpu6050 from "i2c-mpu6050";
import {logMessage} from "../utils";
import {LogLevel} from "../types/global";
import {Sensor} from "../types/sensor";

// Variables
const address: number = 0x68;
const bus: I2CBus = i2c.openSync(1);
const sensor = new Mpu6050(bus, address);
const temperatures: Array<number> = [];
let reverse: number;

/**
 * Reduces all decimals found in the object
 *
 * @param {any} data - Object containing floats
 * @returns {void}
 */
function limitDecimals(data: any): void {
    for (const key in data) {
        if (typeof data[key] === "object") {
            limitDecimals(data[key]);
        } else {
            data[key] = parseFloat(data[key].toFixed(2));
        }
    }
}

/**
 * Smoothes 25 lasts temperature values
 *
 * @param {number} newTemp - Value to be added to the average
 * @returns {number} Average temperature
 */
function averageTemperature(newTemp: number): number {
    temperatures.push(newTemp);
    if (temperatures.length > 25) {
        temperatures.shift();
    }
    const sum: number = temperatures.reduce((a: number, b: number): number => a + b, 0);
    const average: number = sum / temperatures.length;
    return parseFloat(average.toFixed(2));
}

/**
 * Read datas from the sensor
 *
 * @returns {Promise<void>}
 */
async function getSensorDatas(): Promise<void> {
    const data: Sensor = sensor.readSync();

    // Remove unused values
    delete data.gyro;
    delete data.rotation;

    // Offset calibration
    data.accel.x -= 0.058978759765625;
    data.accel.y -= 0.0088987060546875;
    data.accel.z -= 0.059643090820312494;

    // Transform data
    limitDecimals(data);
    data.temp = averageTemperature(data.temp);

    // Invert accel y and z
    reverse = data.accel.z * -1;
    data.accel.z = data.accel.y;
    data.accel.y = reverse;

    // Show / Send data
    if (process.send) {
        process.send({Sensor: data});
    } else {
        logMessage(JSON.stringify({Sensor: data}), LogLevel.DATA);
    }
}

setInterval(getSensorDatas, 125);
