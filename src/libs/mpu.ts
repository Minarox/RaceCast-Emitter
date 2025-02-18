import i2c from "i2c-bus";
import Mpu6050 from "i2c-mpu6050";

// Define global variables
const address: number = 0x68;
const bus: number = i2c.openSync(1);
const mpu6050: Mpu6050 = new Mpu6050(bus, address);
const temperatures: number[] = [];
let oldTemperature: string = '';

// Smooth temperature values
function averageTemperature(): void {
  temperatures.push(mpu6050.readTempSync());

  if (temperatures.length > 30) temperatures.shift();
  const sum: number = temperatures.reduce((a: number, b: number) => a + b, 0);
  const average: string = (sum / temperatures.length).toFixed(1);

  if (oldTemperature !== average) {
    oldTemperature = average;
    process.stdout.write(average);
  }
}

setInterval(averageTemperature, 100);
