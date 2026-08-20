package main

// DESK-2615: tests for processAlive, the parent-pid watchdog primitive behind
// `ios server --parent-pid` (server_parent_unix.go / server_parent_windows.go).
// Platform-neutral: exercises whichever build-tagged implementation compiled in.

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestProcessAliveForCurrentProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatalf("processAlive(self=%d) = false, want true", os.Getpid())
	}
}

func TestProcessAliveForExitedProcess(t *testing.T) {
	// Start a process that exits immediately, reap it, then confirm its pid is
	// reported as gone — the condition the watchdog uses to shut the server down.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "0")
	} else {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process did not exit cleanly: %v", err)
	}

	if processAlive(pid) {
		t.Fatalf("processAlive(exited pid %d) = true, want false", pid)
	}
}
