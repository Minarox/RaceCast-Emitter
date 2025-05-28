// @ts-ignore
import Mpu6050 from "i2c-mpu6050";
import i2c, { type I2CBus } from "i2c-bus";

const address: number = 0x68;
const bus: I2CBus = i2c.openSync(1);
const mpu6050: Mpu6050 = new Mpu6050(bus, address);
const temperatures: number[] = [];
let oldAverage: string = '';

/**
 * Create an average temperature with the last 30 readings.
 *
 * @param {number} temperature - The temperature to add to the array.
 * @returns {string} - The average temperature rounded to one decimal place.
 */
function averageTemperature(temperature: number): string {
    temperatures.push(temperature);

    if (temperatures.length > 30) temperatures.shift();
    const sum: number = temperatures.reduce((a: number, b: number) => a + b, 0);

    return (sum / temperatures.length).toFixed(1);
}

// Start reading the temperature every 100 milliseconds
setInterval(() => {
    const temperature: number = mpu6050.readTempSync();
    const newAverage: string = averageTemperature(temperature);

    if (oldAverage !== newAverage) {
        oldAverage = newAverage;
        process.stdout.write(newAverage);
    }
}, 100);
