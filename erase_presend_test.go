package main

// DESK-3471: the /erase pre-send deadline. The device calls are swapped for fakes, so these
// run without usbmuxd or a device.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

type fakeServiceConn struct{ closed int32 }

func (c *fakeServiceConn) Close() error { atomic.AddInt32(&c.closed, 1); return nil }

// withEraseSeams swaps the device calls and the timeout for one test and restores them after.
func withEraseSeams(t *testing.T, timeout time.Duration,
	resolve func(string) (ios.DeviceEntry, error),
	open func(ios.DeviceEntry) (eraseServiceConn, error),
	send func(eraseServiceConn, ios.DeviceEntry) error) {
	t.Helper()
	oldTimeout, oldResolve, oldOpen, oldSend := erasePresendTimeout, eraseResolveDevice, eraseOpenService, eraseSend
	erasePresendTimeout, eraseResolveDevice, eraseOpenService, eraseSend = timeout, resolve, open, send
	t.Cleanup(func() {
		erasePresendTimeout, eraseResolveDevice, eraseOpenService, eraseSend = oldTimeout, oldResolve, oldOpen, oldSend
	})
}

func postErase(t *testing.T, udid string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	eraseHandler(rec, httptest.NewRequest(http.MethodPost, "/erase?udid="+udid, nil))
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	return rec, body
}

func okResolve(string) (ios.DeviceEntry, error) { return ios.DeviceEntry{}, nil }

func TestEraseHappyPathSendsAndClosesTheConnection(t *testing.T) {
	conn := &fakeServiceConn{}
	sent := int32(0)
	withEraseSeams(t, time.Second, okResolve,
		func(ios.DeviceEntry) (eraseServiceConn, error) { return conn, nil },
		func(eraseServiceConn, ios.DeviceEntry) error { atomic.AddInt32(&sent, 1); return nil })

	rec, body := postErase(t, "happy")
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("status=%d body=%v, want 200 ok", rec.Code, body)
	}
	if atomic.LoadInt32(&sent) != 1 || atomic.LoadInt32(&conn.closed) != 1 {
		t.Fatalf("sent=%d closed=%d, want 1 and 1", sent, conn.closed)
	}
}

// The DESK-3471 hang: opening the service never returns. The route must answer within the
// deadline with notSent=true and the stage it was stuck in, and must not send anything.
func TestEraseOpenServiceHangIsReportedAsNotSent(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeServiceConn{}
	sent := int32(0)
	withEraseSeams(t, 50*time.Millisecond, okResolve,
		func(ios.DeviceEntry) (eraseServiceConn, error) { <-release; return conn, nil },
		func(eraseServiceConn, ios.DeviceEntry) error { atomic.AddInt32(&sent, 1); return nil })

	start := time.Now()
	rec, body := postErase(t, "hang-open")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("route took %v; the deadline did not bound it", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable || body["notSent"] != true || body["stage"] != stageOpenService {
		t.Fatalf("status=%d body=%v, want 503 notSent=true stage=%s", rec.Code, body, stageOpenService)
	}

	// The stuck attempt finishes later: its connection is closed unused and nothing is sent.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&conn.closed) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&conn.closed) != 1 {
		t.Fatal("the late connection was not closed")
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Fatal("a timed-out attempt must never send the erase")
	}
}

func TestEraseResolveDeviceHangIsReportedAsNotSent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	withEraseSeams(t, 50*time.Millisecond,
		func(string) (ios.DeviceEntry, error) { <-release; return ios.DeviceEntry{}, nil },
		func(ios.DeviceEntry) (eraseServiceConn, error) { return &fakeServiceConn{}, nil },
		func(eraseServiceConn, ios.DeviceEntry) error { return nil })

	rec, body := postErase(t, "hang-resolve")
	if rec.Code != http.StatusServiceUnavailable || body["notSent"] != true || body["stage"] != stageResolveDevice {
		t.Fatalf("status=%d body=%v, want 503 notSent=true stage=%s", rec.Code, body, stageResolveDevice)
	}
}

// Fast failures keep their old meaning (404 / 500) and are not "notSent": PDD already
// handles them by trying its next backend.
func TestEraseFastFailuresKeepTheirOldStatus(t *testing.T) {
	withEraseSeams(t, time.Second,
		func(string) (ios.DeviceEntry, error) { return ios.DeviceEntry{}, errors.New("not found") },
		func(ios.DeviceEntry) (eraseServiceConn, error) { return nil, nil },
		func(eraseServiceConn, ios.DeviceEntry) error { return nil })
	rec, body := postErase(t, "missing")
	if rec.Code != http.StatusNotFound || body["notSent"] != nil {
		t.Fatalf("status=%d body=%v, want 404 without notSent", rec.Code, body)
	}

	withEraseSeams(t, time.Second, okResolve,
		func(ios.DeviceEntry) (eraseServiceConn, error) { return nil, errors.New("service refused") },
		func(eraseServiceConn, ios.DeviceEntry) error { return nil })
	rec, body = postErase(t, "refused")
	if rec.Code != http.StatusInternalServerError || body["notSent"] != nil {
		t.Fatalf("status=%d body=%v, want 500 without notSent", rec.Code, body)
	}
}

// A device that keeps hanging must not get an unbounded pile of stuck connections: past the
// limit the route answers notSent at once without opening another one.
func TestEraseStopsOpeningConnectionsToADeviceThatKeepsHanging(t *testing.T) {
	release := make(chan struct{})
	opened := int32(0)
	withEraseSeams(t, 20*time.Millisecond, okResolve,
		func(ios.DeviceEntry) (eraseServiceConn, error) {
			atomic.AddInt32(&opened, 1)
			<-release
			return &fakeServiceConn{}, nil
		},
		func(eraseServiceConn, ios.DeviceEntry) error { return nil })

	udid := "keeps-hanging"
	for i := 0; i < maxStuckErasePresendsPerDevice; i++ {
		if rec, _ := postErase(t, udid); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status=%d, want 503", i, rec.Code)
		}
	}
	rec, body := postErase(t, udid)
	if rec.Code != http.StatusServiceUnavailable || body["stage"] != "stuck-limit" {
		t.Fatalf("status=%d body=%v, want 503 stage=stuck-limit", rec.Code, body)
	}
	if got := atomic.LoadInt32(&opened); got != maxStuckErasePresendsPerDevice {
		t.Fatalf("opened %d connections, want %d", got, maxStuckErasePresendsPerDevice)
	}

	// Once the stuck attempts finish, the device is tried again.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(stuckCounter(udid)) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(stuckCounter(udid)) != 0 {
		t.Fatal("stuck counter did not drain after the attempts finished")
	}
}

func TestEraseRejectsNonPost(t *testing.T) {
	rec := httptest.NewRecorder()
	eraseHandler(rec, httptest.NewRequest(http.MethodGet, "/erase", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
}
