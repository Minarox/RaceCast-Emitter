import { spawn } from "child_process";

function start() {
  const srt = spawn(
    "srt-live-transmit",
    [
      "-t",
      "-1",
      "-a",
      "yes",
      "udp://localhost:5000?pkt_size=1316",
      "srt://212.227.30.91:4200?mode=caller&latency=1000",
    ],
    { shell: true },
  );

  srt.stderr.on("data", (output) => {
    output = output.toString().trim();
    if (output.startsWith("SRT target")) console.log(output.split(" ")[2]);
  });

  srt.on("close", () => {
    start();
  });
}

start();
