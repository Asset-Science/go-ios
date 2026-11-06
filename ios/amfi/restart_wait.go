package amfi

import (
	"errors"
	"fmt"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

// restartWait bounds the wait for the restart that enabling Developer Mode triggers. Every
// field has a default, so the zero value is usable. The defaults add up to 160 s, which keeps
// `devmode enable --enable-post-restart` under a 180 s subprocess cap.
type restartWait struct {
	// Gone is how long the device may stay listed after the enable before it drops off. The
	// device keeps answering for several seconds after the enable, so seeing it listed says
	// nothing until it has gone.
	Gone time.Duration
	// Back is how long the device may take to be listed again once it has gone.
	Back time.Duration
	// PostRestart is how long the post-restart step is retried: the device is listed before
	// its services answer.
	PostRestart time.Duration
	// Poll is the pause between two looks at the device list.
	Poll time.Duration
}

func (w restartWait) withDefaults() restartWait {
	if w.Gone <= 0 {
		w.Gone = 30 * time.Second
	}
	if w.Back <= 0 {
		w.Back = 90 * time.Second
	}
	if w.PostRestart <= 0 {
		w.PostRestart = 40 * time.Second
	}
	if w.Poll <= 0 {
		w.Poll = time.Second
	}
	return w
}

// waitForRestart waits for the device to drop off the device list and then to come back, and
// returns its new entry. A device that never drops off within w.Gone has not restarted, which
// is an error rather than a success.
func waitForRestart(listed func() (ios.DeviceEntry, bool), w restartWait, now func() time.Time, sleep func(time.Duration)) (ios.DeviceEntry, error) {
	w = w.withDefaults()
	goneBy := now().Add(w.Gone)
	for {
		if _, ok := listed(); !ok {
			break
		}
		if !now().Before(goneBy) {
			return ios.DeviceEntry{}, fmt.Errorf("device did not restart within %s of enabling developer mode", w.Gone)
		}
		sleep(w.Poll)
	}
	backBy := now().Add(w.Back)
	for {
		if device, ok := listed(); ok {
			return device, nil
		}
		if !now().Before(backBy) {
			return ios.DeviceEntry{}, errors.New("device was not back within " + w.Back.String() + " of restarting")
		}
		sleep(w.Poll)
	}
}

// retryUntil calls fn until it succeeds or limit has passed, pausing interval between calls,
// and returns fn's last error.
func retryUntil(fn func() error, limit, interval time.Duration, now func() time.Time, sleep func(time.Duration)) error {
	deadline := now().Add(limit)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if !now().Add(interval).Before(deadline) {
			return err
		}
		sleep(interval)
	}
}
