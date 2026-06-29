//go:build windows

package main

import "golang.org/x/sys/windows"

// processAlive reports whether the process with the given pid is still running.
// DESK-2615: used by `ios server --parent-pid` so the server exits when the app
// that launched it goes away.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
