package main

// DESK-3471: bound the part of `/erase` that runs BEFORE anything is sent to the device.
//
// MacPDD reproduced this four times (2.5.92, 2.5.94 x2, 2.5.100): the first erases of a
// 12-device batch blocked inside this route for 2-6 minutes before go-ios logged
// "start erasing", then finished in seconds once their connection dropped (an operator
// re-seating the phones, or PDD's 180 s request timeout). Resolving the device and opening
// the MCInstall service (usbmuxd device list, lockdown session, StartService, the service
// connection) has no deadline anywhere, so a connection that stops answering parks the
// request until that connection dies. PDD holds a station-wide USB-disruptive permit
// around the erase, so every other slot queued behind the stuck ones.
//
// Nothing has been sent to the device until mcinstall.EraseWithConnection runs. So this
// phase is bounded, and a timeout is answered with 503 {"notSent": true, "stage": ...}:
// an explicit "nothing happened, retrying is safe", which PDD uses to give its permit back
// and try again instead of holding the queue. The stuck attempt is left to finish on its
// own; if it ever connects, the connection is closed unused.
//
// The stage durations are logged on every erase, so the next macOS reproduction says
// exactly which step blocked: resolving the device or opening the service.

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
	"github.com/danielpaulus/go-ios/ios/mcinstall"
)

const erasePresendModule = "go-ios/server"

// How long resolving the device and opening MCInstall may take before the erase is reported
// as not sent. Healthy attempts take well under a second; a hung one took 134-338 s.
var erasePresendTimeout = 30 * time.Second

// At most this many timed-out pre-send attempts may still be stuck for one device. Beyond
// that the route answers "not sent" at once instead of opening yet another connection to a
// device that is not answering.
const maxStuckErasePresendsPerDevice = 3

// Seams for the tests: the real calls need usbmuxd and an attached device.
type eraseServiceConn interface{ Close() error }

var (
	eraseResolveDevice = ios.GetDevice
	eraseOpenService   = func(d ios.DeviceEntry) (eraseServiceConn, error) {
		conn, err := mcinstall.New(d)
		if err != nil {
			return nil, err // mcinstall.New returns an empty, unclosable Connection on error
		}
		return conn, nil
	}
	eraseSend = func(c eraseServiceConn, d ios.DeviceEntry) error {
		conn, ok := c.(*mcinstall.Connection)
		if !ok {
			return errors.New("erase: unexpected service connection type")
		}
		return mcinstall.EraseWithConnection(conn, d)
	}
)

var stuckErasePresends sync.Map // udid -> *int32 count of timed-out attempts still running

func stuckCounter(udid string) *int32 {
	v, _ := stuckErasePresends.LoadOrStore(udid, new(int32))
	return v.(*int32)
}

type erasePresend struct {
	device          ios.DeviceEntry
	conn            eraseServiceConn
	stage           string
	err             error
	resolveDuration time.Duration
	openDuration    time.Duration
}

const (
	stageResolveDevice = "resolve-device"
	stageOpenService   = "open-service"
)

func eraseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errBadRequest("POST required"))
		return
	}
	udid := r.URL.Query().Get("udid")
	stuck := stuckCounter(udid)
	if atomic.LoadInt32(stuck) >= maxStuckErasePresendsPerDevice {
		golog.Warn("erase not attempted: earlier attempts for this device are still stuck before sending",
			"module", erasePresendModule, "udid", udid, "stuck", atomic.LoadInt32(stuck))
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "earlier erase attempts for this device are still waiting on the device; nothing was sent",
			"notSent": true,
			"stage":   "stuck-limit",
		})
		return
	}

	var stage atomic.Value
	stage.Store(stageResolveDevice)
	done := make(chan erasePresend, 1)
	started := time.Now()
	go func() {
		t0 := time.Now()
		device, err := eraseResolveDevice(udid)
		resolved := time.Since(t0)
		if err != nil {
			done <- erasePresend{stage: stageResolveDevice, err: err, resolveDuration: resolved}
			return
		}
		stage.Store(stageOpenService)
		t1 := time.Now()
		conn, err := eraseOpenService(device)
		done <- erasePresend{device: device, conn: conn, stage: stageOpenService, err: err,
			resolveDuration: resolved, openDuration: time.Since(t1)}
	}()

	timer := time.NewTimer(erasePresendTimeout)
	defer timer.Stop()
	select {
	case p := <-done:
		golog.Info("erase pre-send finished", "module", erasePresendModule, "udid", udid,
			"resolveDeviceMs", p.resolveDuration.Milliseconds(), "openServiceMs", p.openDuration.Milliseconds(),
			"ok", p.err == nil)
		if p.err != nil {
			// Unchanged from the old route: a device that cannot be found is 404, any other
			// failure to open the service is 500. These fail fast, so they are not "notSent".
			status := http.StatusInternalServerError
			if p.stage == stageResolveDevice {
				status = http.StatusNotFound
			}
			writeErr(w, status, p.err)
			return
		}
		defer p.conn.Close()
		if err := eraseSend(p.conn, p.device); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	case <-timer.C:
		blockedIn, _ := stage.Load().(string)
		atomic.AddInt32(stuck, 1)
		golog.Warn("erase pre-send timed out; nothing was sent to the device",
			"module", erasePresendModule, "udid", udid, "stage", blockedIn,
			"elapsedMs", time.Since(started).Milliseconds())
		go func() {
			p := <-done
			atomic.AddInt32(stuck, -1)
			if p.conn != nil {
				_ = p.conn.Close()
			}
			golog.Info("timed-out erase pre-send finished late; its connection was closed unused",
				"module", erasePresendModule, "udid", udid, "stage", p.stage,
				"resolveDeviceMs", p.resolveDuration.Milliseconds(), "openServiceMs", p.openDuration.Milliseconds(),
				"ok", p.err == nil)
		}()
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "the device did not answer before the erase was sent; nothing was sent",
			"notSent": true,
			"stage":   blockedIn,
		})
	}
}
