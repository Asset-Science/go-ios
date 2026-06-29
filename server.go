package main

// DESK-2615: `ios server` — a single long-lived go-ios instance that exposes the
// device operations PDD needs over a small stdlib net/http REST API, instead of
// the app spawning a fresh `ios <cmd>` subprocess per call. One process, one
// usbmux/lockdown context, no per-call cold start.
//
// Deliberately stdlib-only (no gin/router dep): the heavier restapi/ module is a
// separate go module aimed at WDA automation; this server lives in the main
// binary so there is exactly one `ios` executable to ship.
//
// Devices are selected with a `udid` query parameter; an empty/absent udid uses
// the single attached device (ios.GetDevice("")). Every response is JSON:
// success returns the payload, failure returns {"error": "..."} with HTTP 4xx/5xx.

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/diagnostics"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/mcinstall"
	"github.com/danielpaulus/go-ios/ios/mobileactivation"
	"github.com/danielpaulus/go-ios/ios/zipconduit"
	log "github.com/sirupsen/logrus"
)

// runServer starts the REST daemon and blocks until the process is killed.
func runServer(address string) {
	if address == "" {
		address = "127.0.0.1:8080"
	}

	mux := http.NewServeMux()

	// --- liveness ---
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "version": GetVersionFallback()})
	})

	// --- device discovery (no udid required) ---
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		list, err := ios.ListDevices()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		udids := make([]string, 0, len(list.DeviceList))
		for _, d := range list.DeviceList {
			udids = append(udids, d.Properties.SerialNumber)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deviceList": udids})
	})

	// --- read-only device queries ---
	mux.HandleFunc("/info", deviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		values, err := ios.GetValues(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, values.Value)
	}))

	mux.HandleFunc("/pair/check", deviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		paired, err := ios.IsPaired(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"Paired": paired})
	}))

	mux.HandleFunc("/apps", deviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		svc, err := installationproxy.New(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		defer svc.Close()
		var apps []installationproxy.AppInfo
		if r.URL.Query().Get("all") == "true" {
			apps, err = svc.BrowseAllApps()
		} else {
			apps, err = svc.BrowseUserApps()
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, apps)
	}))

	mux.HandleFunc("/profiles", deviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		conn, err := mcinstall.New(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		profiles, err := conn.HandleList()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, profiles)
	}))

	mux.HandleFunc("/batterycheck", deviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		battery, err := ios.GetBatteryDiagnostics(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, battery)
	}))

	// --- device actions (POST) ---
	mux.HandleFunc("/reboot", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		if err := diagnostics.Reboot(d); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	mux.HandleFunc("/install", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeErr(w, http.StatusBadRequest, errBadRequest("missing 'path' (ipa or .app folder)"))
			return
		}
		conn, err := zipconduit.New(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if err := conn.SendFile(path); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	mux.HandleFunc("/uninstall", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		bundleID := r.URL.Query().Get("bundleId")
		if bundleID == "" {
			writeErr(w, http.StatusBadRequest, errBadRequest("missing 'bundleId'"))
			return
		}
		svc, err := installationproxy.New(d)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		defer svc.Close()
		if err := svc.Uninstall(bundleID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	mux.HandleFunc("/activate", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		if err := mobileactivation.Activate(d); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	mux.HandleFunc("/erase", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		if err := mcinstall.Erase(d); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	// prepare (supervised cloud-config + skip-setup). chooseLocationServices is
	// implicit in the skip list; pass certfile (DER or p12) + orgname to supervise.
	mux.HandleFunc("/prepare", postDeviceHandler(func(w http.ResponseWriter, r *http.Request, d ios.DeviceEntry) {
		q := r.URL.Query()
		// chooseLocation=true shows the Location pane (skip everything else);
		// otherwise skip every setup pane. Mirrors the PDD cfgutil behavior.
		skip := mcinstall.GetAllSetupSkipOptions()
		if q.Get("chooseLocation") == "true" {
			filtered := skip[:0:0]
			for _, k := range skip {
				if k != "Location" {
					filtered = append(filtered, k)
				}
			}
			skip = filtered
		}
		var certBytes []byte
		certfile := q.Get("certfile")
		orgname := q.Get("orgname")
		if certfile != "" {
			raw, err := os.ReadFile(certfile)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			certBytes, err = extractDERCertificate(raw, q.Get("p12password"))
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
		if err := mcinstall.Prepare(d, skip, certBytes, orgname, q.Get("locale"), q.Get("lang")); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeOK(w)
	}))

	srv := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.WithFields(log.Fields{"address": address}).Info("go-ios server listening")
	if err := srv.ListenAndServe(); err != nil {
		log.WithError(err).Fatal("go-ios server failed")
	}
}

// deviceHandler resolves the device from the `udid` query param (empty => the
// single attached device) and passes it to fn. Used for GET endpoints.
func deviceHandler(fn func(http.ResponseWriter, *http.Request, ios.DeviceEntry)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device, err := ios.GetDevice(r.URL.Query().Get("udid"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		fn(w, r, device)
	}
}

// postDeviceHandler is deviceHandler that additionally rejects non-POST methods.
func postDeviceHandler(fn func(http.ResponseWriter, *http.Request, ios.DeviceEntry)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, errBadRequest("POST required"))
			return
		}
		device, err := ios.GetDevice(r.URL.Query().Get("udid"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		fn(w, r, device)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]interface{}{"error": err.Error()})
}

func writeOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errBadRequest(msg string) error { return simpleError(msg) }

// GetVersionFallback returns the build version if available, else "dev".
func GetVersionFallback() string {
	if v := os.Getenv("GO_IOS_VERSION"); v != "" {
		return v
	}
	return "dev"
}
