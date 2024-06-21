import { spawn } from "child_process";
import fs from "fs";

const usbPath = `/sys/bus/usb/devices/6-1/authorized`;
let counter = 0;
let online = false;

function checkDevice() {
  return new Promise((resolve) => {
    const lsusb = spawn("lsusb");
    let data = "";

    lsusb.stdout.on("data", (output) => {
      data += output.toString();
    });

    lsusb.on("close", () => {
      resolve(data.trim().includes("Elgato"));
    });
  });
}

function togglePort(boolean) {
  fs.writeFile(usbPath, Number(boolean).toString(), () => {});
}

function startFFmpeg() {
  const ffmpeg = spawn(
    "ffmpeg",
    [
      "-hide_banner",
      "-f",
      "v4l2",
      "-thread_queue_size",
      "1024",
      "-c:v",
      "rawvideo",
      "-video_size",
      "1920x1080",
      "-framerate",
      "50",
      "-pixel_format",
      "yuyv422",
      "-i",
      "/dev/video20",
      "-f",
      "alsa",
      "-thread_queue_size",
      "1024",
      "-c:a",
      "pcm_s16le",
      "-ac",
      "2",
      "-i",
      "hw:CARD=C4K,DEV=0",
      "-c:v",
      "libx264",
      "-s",
      "852x480",
      "-pix_fmt",
      "yuv420p",
      "-filter:v",
      "fps=25",
      "-r",
      "25",
      "-q",
      "50",
      "-b:v",
      "400k",
      "-maxrate",
      "500k",
      "-bufsize",
      "1000k",
      "-preset",
      "veryfast",
      "-tune",
      "zerolatency",
      "-c:a",
      "aac",
      "-ar",
      "44100",
      "-ab",
      "128k",
      "-f",
      "mpegts",
      "-omit_video_pes_length",
      "1",
      "udp://127.0.0.1:5000?pkt_size=1316",
    ],
    { shell: true },
  );

  ffmpeg.stderr.on("data", (output) => {
    output = output.toString();
    if (output.startsWith("frame=")) {
      if (counter < 2) counter++;
      else if (!online) {
        if (parseFloat(output.split(" ")[5]) <= 1.0) ffmpeg.kill();
        console.log("online");
        online = true;
      }
    }
  });

  ffmpeg.on("close", () => {
    console.log("offline");
    counter = 0;
    online = false;
    setTimeout(() => {
      start();
    }, 1000);
  });
}

function start() {
  checkDevice().then((status) => {
    if (status) {
      togglePort(false);
      setTimeout(() => {
        togglePort(true);
        setTimeout(() => {
          startFFmpeg();
        }, 10000);
      }, 1000);
    } else {
      setTimeout(() => {
        start();
      }, 2000);
    }
  });
}

start();
