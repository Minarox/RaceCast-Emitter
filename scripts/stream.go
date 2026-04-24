package scripts

import (
	"os/exec"
	"racecast-emitter/utils"
)

func ListDevices() string {
	output, _ := exec.Command("sh", "-c", `ls /dev/video*`).Output()
	utils.Log.Infow("Available video devices.", "output", string(output))
	
	return string(output)
}
