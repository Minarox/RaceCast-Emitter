import i2c, {I2CBus} from "i2c-bus";
import Mpu6050 from "i2c-mpu6050";

// Define global variables
const address: number = 0x68;
const bus: I2CBus = i2c.openSync(1);
const sensor = new Mpu6050(bus, address);
const temperatures: Array<number> = [];
let reverse: number;

// Limit all values to 2 decimals
function limitDecimals(data: any): void {
    for (const key in data) {
        if (typeof data[key] === "object") {
            limitDecimals(data[key]);
        } else {
            data[key] = parseFloat(data[key].toFixed(2));
        }
    }
}

// Smooth temperature values
function averageTemperature(newTemp: number): number {
    temperatures.push(newTemp);
    if (temperatures.length > 25) {
        temperatures.shift();
    }
    const sum: number = temperatures.reduce((a: number, b: number): number => a + b, 0);
    const average: number = sum / temperatures.length;
    return parseFloat(average.toFixed(2));
}

// Read sensor data
export default function getSensorDatas(): void {
    const data: any = sensor.readSync();
    delete data.gyro.z;

    // Offset calibration
    data.accel.x -= 0.058978759765625;
    data.accel.y -= 0.0088987060546875;
    data.accel.z -= 0.059643090820312494;
    data.gyro.x -= -0.7022061068702323;
    data.gyro.y -= 1.0760305343511471;
    data.rotation.x -= -0.4804232806148877;
    data.rotation.y -= -3.1856752923673435;

    // Transform data
    limitDecimals(data);
    data.temp = averageTemperature(data.temp);

    // Invert gyro x and y
    reverse = data.gyro.x * -1;
    data.gyro.x = data.gyro.y;
    data.gyro.y = reverse;

    // Invert accel y and z
    reverse = data.accel.z * -1;
    data.accel.z = data.accel.y;
    data.accel.y = reverse;

    // Show data
    process.stdout.write(JSON.stringify(data));
}
