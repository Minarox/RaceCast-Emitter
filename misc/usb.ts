import { spawn } from "child_process";
import fs from "fs";

const usbPath: string = `/sys/bus/usb/devices/6-1/authorized`;
const command: string = process.argv[2] || '';
let prevent: boolean = false;

function toggle(boolean: boolean): void {
    fs.writeFile(usbPath, Number(boolean).toString(), () => {});
}

function done(): void {
    setTimeout(() => {
        console.log("done");
    }, 1000);
}

switch (command) {
    case "status":
        prevent = true;
        const lsusb = spawn("lsusb");
        let data: string = "";
        
        lsusb.stdout.on("data", (output: string): void => {
            data += output.toString();
        });
        
        lsusb.on("close", (): void => {
            if (data.trim().includes("Elgato")) console.log("connected");
            else console.log("disconnected");
        });
        break;

    case "on":
        toggle(true);
        break;

    case "off":
        toggle(false);
        break;

    case "restart":
        prevent = true;
        toggle(false);
        setTimeout(() => {
            toggle(true);
            done();
        }, 1000);
        break;

    default:
        prevent = true;
        console.log("Invalid command. Usage: node usb.js [status|on|off|restart]");
        break;
}

if (!prevent) done();
