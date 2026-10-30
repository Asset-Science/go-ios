package main

// DESK-3471: bound the part of an erase that runs BEFORE anything is sent to the device.
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
// phase is bounded, and a timeout is answered with {"notSent": true, "stage": ...}: an
// explicit "nothing happened, retrying is safe", which PDD uses to give its permit back and
// try again instead of holding the queue. The route answers 503 with it; `ios erase` prints
// it and exits eraseNotSentExitCode. On the route the stuck attempt is left to finish on its
// own and, if it ever connects, its connection is closed unused; on the CLI it dies with the
// process.
//
// This frees the queue; it does not by itself un-stick the phone. A phone whose connection
// has stopped answering keeps not answering until that connection dies (an operator
// re-seating it is what ended every reproduction), so PDD keeps retrying it, unbounded,
// without holding the permit in between.
//
// The stage durations are logged on every erase, so the next macOS reproduction says
// exactly which step blocked: resolving the device or opening the service.

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
	"github.com/danielpaulus/go-ios/ios/mcinstall"
)

const erasePresendModule = "go-ios/erase"

// How long resolving the device and opening MCInstall may take before the erase is reported
// as not sent. The hung attempts in the field took 134-338 s; a healthy one is a usbmuxd
// lookup plus a lockdown session, so 30 s is generous headroom that still frees the queue.
var erasePresendTimeout = 30 * time.Second

// At most this many timed-out pre-send attempts may still be stuck for one device. Beyond
// that the route answers "not sent" at once instead of opening yet another connection to a
// device that is not answering.
const maxStuckErasePresendsPerDevice = 3

// What `ios erase` exits with when it gave up before sending anything: EX_TEMPFAIL from
// sysexits.h ("try again later"). PDD keys on the JSON payload on stdout, which says the
// same thing; the exit code is for shell callers.
const eraseNotSentExitCode = 75

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
	// os.Exit and stdout for `ios erase`, swapped by the tests.
	eraseCLIExit   = os.Exit
	eraseCLIStdout = func(s string) { fmt.Print(s) }
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

const notSentTimeoutMsg = "the device did not answer before the erase was sent; nothing was sent"

func notSentPayload(msg, stage string) map[string]interface{} {
	return map[string]interface{}{"error": msg, "notSent": true, "stage": stage}
}

// awaitErasePresend resolves the device and opens MCInstall, giving up after
// erasePresendTimeout. When it gives up, late is non-nil and delivers the stuck attempt's
// result whenever it finishes, and blockedIn names the stage that was still running.
func awaitErasePresend(udid string) (p erasePresend, late <-chan erasePresend, blockedIn string) {
	var stage atomic.Value
	stage.Store(stageResolveDevice)
	done := make(chan erasePresend, 1)
	// Read the seams here, on the caller's goroutine, not inside the one below: that goroutine
	// outlives this call when it times out, and must not read package state after we return
	// (the tests restore the seams then; under -race that was a data race).
	resolveDevice, openService := eraseResolveDevice, eraseOpenService
	go func() {
		t0 := time.Now()
		device, err := resolveDevice(udid)
		resolved := time.Since(t0)
		if err != nil {
			done <- erasePresend{stage: stageResolveDevice, err: err, resolveDuration: resolved}
			return
		}
		stage.Store(stageOpenService)
		t1 := time.Now()
		conn, err := openService(device)
		done <- erasePresend{device: device, conn: conn, stage: stageOpenService, err: err,
			resolveDuration: resolved, openDuration: time.Since(t1)}
	}()

	timer := time.NewTimer(erasePresendTimeout)
	defer timer.Stop()
	select {
	case p = <-done:
		golog.Info("erase pre-send finished", "module", erasePresendModule, "udid", udid,
			"resolveDeviceMs", p.resolveDuration.Milliseconds(), "openServiceMs", p.openDuration.Milliseconds(),
			"ok", p.err == nil)
		return p, nil, ""
	case <-timer.C:
		blockedIn, _ = stage.Load().(string)
		return erasePresend{}, done, blockedIn
	}
}

func eraseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errBadRequest("POST required"))
		return
	}
	udid := r.URL.Query().Get("udid")
	stuck := stuckCounter(udid)
	if n := atomic.LoadInt32(stuck); n >= maxStuckErasePresendsPerDevice {
		writeJSON(w, http.StatusServiceUnavailable, notSentPayload(
			"earlier erase attempts for this device are still waiting on the device; nothing was sent",
			"stuck-limit"))
		golog.Warn("erase not attempted: earlier attempts for this device are still stuck before sending",
			"module", erasePresendModule, "udid", udid, "stuck", n)
		return
	}

	started := time.Now()
	p, late, blockedIn := awaitErasePresend(udid)
	if late != nil {
		// Counted before the late-close goroutine can uncount it. The response goes out before
		// the log line so the client is never kept waiting on the log sink.
		atomic.AddInt32(stuck, 1)
		go func() {
			p := <-late
			atomic.AddInt32(stuck, -1)
			if p.conn != nil {
				_ = p.conn.Close()
			}
			golog.Info("timed-out erase pre-send finished late; its connection was closed unused",
				"module", erasePresendModule, "udid", udid, "stage", p.stage,
				"resolveDeviceMs", p.resolveDuration.Milliseconds(), "openServiceMs", p.openDuration.Milliseconds(),
				"ok", p.err == nil)
		}()
		writeJSON(w, http.StatusServiceUnavailable, notSentPayload(notSentTimeoutMsg, blockedIn))
		golog.Warn("erase pre-send timed out; nothing was sent to the device",
			"module", erasePresendModule, "udid", udid, "stage", blockedIn,
			"elapsedMs", time.Since(started).Milliseconds())
		return
	}
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
}

// usesBoundedEraseCLI reports whether `ios erase` should take runBoundedEraseCLI. That covers
// plain usbmuxd erases, which is every erase PDD issues. An erase addressed over RSD or a
// tunnel keeps the device-command path, which needs main()'s tunnel resolution.
func usesBoundedEraseCLI(args map[string]interface{}) bool {
	if v, _ := args["erase"].(bool); !v {
		return false
	}
	for _, k := range []string{"--address", "--rsd-port", "--userspace-port"} {
		if v, ok := args[k]; ok && v != nil && v != false && v != "" {
			return false
		}
	}
	return true
}

// runBoundedEraseCLI is `ios erase` with the route's pre-send deadline. PDD runs the CLI
// whenever its server is not up, and main() used to resolve the device before any command
// ran - outside every deadline - so the lookup is done here instead, under it.
func runBoundedEraseCLI(udid string, force bool) {
	if !force && !confirmErase(udid) {
		return
	}
	p, late, blockedIn := awaitErasePresend(udid)
	if late != nil {
		// Payload first: os.Stdout is unbuffered, so it is out before the process exits.
		eraseCLIStdout(convertToJSONString(notSentPayload(notSentTimeoutMsg, blockedIn)))
		golog.Warn("erase pre-send timed out; nothing was sent to the device",
			"module", erasePresendModule, "udid", udid, "stage", blockedIn)
		eraseCLIExit(eraseNotSentExitCode)
		return
	}
	if p.err != nil {
		if p.stage == stageResolveDevice {
			exitIfError("Device not found: "+udid, p.err)
		}
		exitIfError("failed erasing", p.err)
		return
	}
	err := eraseSend(p.conn, p.device)
	_ = p.conn.Close() // explicit, not deferred: exitIfError exits without running defers
	exitIfError("failed erasing", err)
	eraseCLIStdout(convertToJSONString("ok"))
}

func confirmErase(udid string) bool {
	slog.Warn("are you sure you want to erase device? (y/n)", "udid", udid)
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	exitIfError("An error occured while reading input", err)
	if !strings.HasPrefix(input, "y") {
		slog.Error("abort")
		return false
	}
	return true
}
