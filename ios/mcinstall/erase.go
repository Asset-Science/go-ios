package mcinstall

import (
	"fmt"
	"io"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
)

// Erase tells a device to remove all apps and settings. You need to activate it afterwards.
// Be careful with this if you do not have a backup!
func Erase(device ios.DeviceEntry) error {
	conn, err := New(device)
	if err != nil {
		return err
	}
	defer conn.Close()
	return EraseWithConnection(conn, device)
}

// EraseWithConnection sends the erase over an already-open MCInstall connection.
// Nothing reaches the device before this is called, so a caller that bounds the
// connect phase (DESK-3471: the `ios server` /erase route) can report a timeout
// there as "nothing was sent" and let its caller retry safely. The caller owns conn.
func EraseWithConnection(conn *Connection, device ios.DeviceEntry) error {
	golog.Info("start erasing", "module", logModule, "udid", device.Properties.SerialNumber)
	golog.Debug("send flush request", "module", logModule, "udid", device.Properties.SerialNumber)
	_, err := check(conn.sendAndReceive(request("Flush")))
	if err != nil {
		return err
	}
	golog.Debug("get cloud config", "module", logModule, "udid", device.Properties.SerialNumber)
	config, err := check(conn.sendAndReceive(request("GetCloudConfiguration")))
	if err != nil {
		return err
	}
	golog.Debug("got cloud config", "module", logModule, "udid", device.Properties.SerialNumber, "config", config)

	golog.Debug("send erase request", "module", logModule, "udid", device.Properties.SerialNumber)
	eraseRequest := map[string]interface{}{
		"RequestType":      "EraseDevice",
		"PreserveDataPlan": 1,
	}
	_, err = check(conn.sendAndReceive(eraseRequest))
	if err != nil && err != io.EOF {
		return err
	}
	golog.Info("device should be rebooting now", "module", logModule, "udid", device.Properties.SerialNumber)
	return nil
}

func check(request map[string]interface{}, err error) (map[string]interface{}, error) {
	if err != nil {
		return map[string]interface{}{}, err
	}
	if !checkStatus(request) {
		return map[string]interface{}{}, fmt.Errorf("failed command: %v", request)
	}
	return request, nil
}
