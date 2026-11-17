package amfi

import (
	"errors"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/stretchr/testify/assert"
)

// fakeClock advances only when the code sleeps, so the tests run instantly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

// listedFor reports the device as listed according to a timeline in seconds since start:
// listed before goneAt, absent from goneAt until backAt, listed again from backAt (backAt < 0:
// never back).
func listedFor(c *fakeClock, goneAt, backAt float64) func() (ios.DeviceEntry, bool) {
	start := c.t
	return func() (ios.DeviceEntry, bool) {
		s := c.t.Sub(start).Seconds()
		if s < goneAt || (backAt >= 0 && s >= backAt) {
			return ios.DeviceEntry{DeviceID: 7}, true
		}
		return ios.DeviceEntry{}, false
	}
}

func TestWaitForRestart_stillListedAfterTheEnableIsNotARestart_DESK2464(t *testing.T) {
	// The bench iPhone (iOS 27.0) stayed listed for 17 s after the enable; a 10 s look used to
	// take that for the restart having finished.
	c := &fakeClock{t: time.Unix(0, 0)}
	device, err := waitForRestart(listedFor(c, 17, 60), restartWait{}, c.now, c.sleep)
	assert.NoError(t, err)
	assert.Equal(t, 7, device.DeviceID)
	assert.GreaterOrEqual(t, c.t.Sub(time.Unix(0, 0)), 60*time.Second, "returns only once the device is back")
}

func TestWaitForRestart_neverGoneIsAnError(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	_, err := waitForRestart(listedFor(c, 1000, -1), restartWait{}, c.now, c.sleep)
	assert.ErrorContains(t, err, "did not restart")
	assert.LessOrEqual(t, c.t.Sub(time.Unix(0, 0)), 31*time.Second)
}

func TestWaitForRestart_neverBackIsAnError(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	_, err := waitForRestart(listedFor(c, 5, -1), restartWait{}, c.now, c.sleep)
	assert.ErrorContains(t, err, "not back")
	assert.LessOrEqual(t, c.t.Sub(time.Unix(0, 0)), 96*time.Second)
}

func TestWaitForRestart_defaultsFitUnderTheSubprocessCap(t *testing.T) {
	w := restartWait{}.withDefaults()
	assert.Less(t, w.Gone+w.Back+w.PostRestart, 180*time.Second)
}

func TestRetryUntil_retriesUntilTheServiceAnswers(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	err := retryUntil(func() error {
		calls++
		if calls < 3 {
			return errors.New("service not ready")
		}
		return nil
	}, 40*time.Second, 5*time.Second, c.now, c.sleep)
	assert.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestRetryUntil_returnsTheLastErrorAtTheLimit(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	err := retryUntil(func() error {
		calls++
		return errors.New("service not ready")
	}, 40*time.Second, 5*time.Second, c.now, c.sleep)
	assert.ErrorContains(t, err, "not ready")
	assert.Equal(t, 8, calls)
	assert.Less(t, c.t.Sub(time.Unix(0, 0)), 40*time.Second)
}
