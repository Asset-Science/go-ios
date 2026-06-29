//go:build !windows

package main

import "syscall"

// processAlive reports whether the process with the given pid is still running.
// DESK-2615: used by `ios server --parent-pid` so the server exits when the app
// that launched it goes away. Signal 0 performs no delivery but still does the
// existence/permission check: nil or EPERM => alive, ESRCH => gone.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
