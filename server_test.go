package main

// DESK-2615: unit tests for the `ios server` REST daemon (server.go).
// These cover the device-independent surface — response helpers, the version
// fallback, the /prepare skip-list logic, method enforcement, and the /health
// route wired through the real mux. Endpoints that resolve a device
// (ios.GetDevice) require usbmuxd + an attached device and are exercised by the
// -e2e path, not here.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/mcinstall"
)

func TestWriteJSONSetsStatusContentTypeAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, map[string]interface{}{"hello": "world"})

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if got["hello"] != "world" {
		t.Fatalf("body = %#v, want hello=world", got)
	}
}

func TestWriteErrProducesErrorField(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, errBadRequest("boom"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got["error"] != "boom" {
		t.Fatalf("error = %q, want boom", got["error"])
	}
}

func TestWriteOK(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOK(rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if !got["ok"] {
		t.Fatalf("body = %#v, want ok=true", got)
	}
}

func TestErrBadRequestMessageRoundTrips(t *testing.T) {
	err := errBadRequest("missing 'path'")
	if err.Error() != "missing 'path'" {
		t.Fatalf("Error() = %q, want \"missing 'path'\"", err.Error())
	}
	// simpleError must satisfy the error interface (compile-time via assignment).
	var _ error = simpleError("x")
}

func TestGetVersionFallback(t *testing.T) {
	const key = "GO_IOS_VERSION"
	orig, had := os.LookupEnv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	})

	os.Unsetenv(key)
	if v := GetVersionFallback(); v != "dev" {
		t.Fatalf("with env unset: version = %q, want dev", v)
	}

	os.Setenv(key, "1.2.3")
	if v := GetVersionFallback(); v != "1.2.3" {
		t.Fatalf("with env set: version = %q, want 1.2.3", v)
	}
}

func TestPrepareSkipOptionsSkipAllByDefault(t *testing.T) {
	got := prepareSkipOptions(false)
	want := mcinstall.GetAllSetupSkipOptions()
	if len(got) != len(want) {
		t.Fatalf("skip count = %d, want %d (all options)", len(got), len(want))
	}
	if !contains(got, "Location") {
		t.Fatalf("chooseLocation=false must skip Location; got %v", got)
	}
}

func TestPrepareSkipOptionsKeepsLocationWhenChosen(t *testing.T) {
	all := mcinstall.GetAllSetupSkipOptions()
	got := prepareSkipOptions(true)

	if contains(got, "Location") {
		t.Fatalf("chooseLocation=true must NOT skip Location; got %v", got)
	}
	if len(got) != len(all)-1 {
		t.Fatalf("skip count = %d, want %d (all except Location)", len(got), len(all)-1)
	}
	// Every other option must still be skipped.
	for _, k := range all {
		if k == "Location" {
			continue
		}
		if !contains(got, k) {
			t.Fatalf("option %q should still be skipped; got %v", k, got)
		}
	}
}

// TestPrepareSkipOptionsDoesNotMutateSource guards against the [:0:0] filtering
// aliasing and clobbering the shared slice returned by GetAllSetupSkipOptions.
func TestPrepareSkipOptionsDoesNotMutateSource(t *testing.T) {
	before := mcinstall.GetAllSetupSkipOptions()
	_ = prepareSkipOptions(true)
	after := mcinstall.GetAllSetupSkipOptions()

	if len(before) != len(after) {
		t.Fatalf("source length changed: %d -> %d", len(before), len(after))
	}
	if !contains(after, "Location") {
		t.Fatalf("source no longer contains Location after filtering: %v", after)
	}
}

func TestPostDeviceHandlerRejectsNonPOSTBeforeDeviceResolution(t *testing.T) {
	called := false
	h := postDeviceHandler(func(http.ResponseWriter, *http.Request, ios.DeviceEntry) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/reboot", nil)
	h(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if called {
		t.Fatal("inner handler ran for a non-POST request; method check must short-circuit before device resolution")
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got["error"] == "" {
		t.Fatalf("405 response should carry an error field; got %v", got)
	}
}

func TestHealthEndpointThroughMux(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	newServeMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("health body = %#v, want ok=true", got)
	}
	if _, present := got["version"]; !present {
		t.Fatalf("health body missing version field: %#v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
