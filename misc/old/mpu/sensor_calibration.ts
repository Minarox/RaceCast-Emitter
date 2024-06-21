import i2c from "i2c-bus";
import Mpu6050 from "i2c-mpu6050";

// Init I2C bus with MPU6050 address
const address = 0x68;
const i2c1 = i2c.openSync(1);

// Init MPU6050
const sensor = new Mpu6050(i2c1, address);

// Variables pour la calibration des offsets des capteurs
const calibrationIterations = 10000;
const sumOffsets = {
  accel: { x: 0, y: 0, z: 0 },
  gyro: { x: 0, y: 0, z: 0 },
  rotation: { x: 0, y: 0 },
};
let currentIteration = 0;

// Effectuer la calibration
function calibrate() {
  const data = sensor.readSync();
  sumOffsets.accel.x += data.accel.x;
  sumOffsets.accel.y += data.accel.y;
  sumOffsets.accel.z += data.accel.z;
  sumOffsets.gyro.x += data.gyro.x;
  sumOffsets.gyro.y += data.gyro.y;
  sumOffsets.gyro.z += data.gyro.z;
  sumOffsets.rotation.x += data.rotation.x;
  sumOffsets.rotation.y += data.rotation.y;
  currentIteration++;

  // Afficher le nombre d'itérations
  console.clear();
  console.log(
    `Calibration en cours - Itération ${currentIteration}/${calibrationIterations}`,
  );

  // Arrêter le programme après la 1000ème itération
  if (currentIteration === calibrationIterations) {
    clearInterval(calibrationInterval);
    finishCalibration();
  }
}

// Fonction pour calculer les offsets moyens
function calculateOffsets() {
  return {
    accel: {
      x: sumOffsets.accel.x / calibrationIterations,
      y: sumOffsets.accel.y / calibrationIterations,
      z: sumOffsets.accel.z / calibrationIterations,
    },
    gyro: {
      x: sumOffsets.gyro.x / calibrationIterations,
      y: sumOffsets.gyro.y / calibrationIterations,
      z: sumOffsets.gyro.z / calibrationIterations,
    },
    rotation: {
      x: sumOffsets.rotation.x / calibrationIterations,
      y: sumOffsets.rotation.y / calibrationIterations,
    },
  };
}

// Afficher les offsets définitifs
function displayOffsets(offsets) {
  // Remove gravitational acceleration from z-axis accelerometer value
  offsets.accel.z -= 1;

  console.clear();
  console.log("Offsets:", offsets);
}

// Fonction pour terminer la calibration
function finishCalibration() {
  // Calculer et afficher les offsets définitifs
  const offsets = calculateOffsets();
  displayOffsets(offsets);

  // Terminer le programme
  process.exit();
}

// Lancer la calibration
const calibrationInterval = setInterval(calibrate, 100);
