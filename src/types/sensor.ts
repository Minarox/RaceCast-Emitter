export interface Sensor {
    accel: {
        x: number;
        y: number;
        z?: number;
    };
    gyro?: {
        x: number;
        y: number;
        z: number;
    };
    rotation?: {
        x: number;
        y: number;
    },
    temp: number;
}
