//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunIdleTimeoutKillsProcessGroup(t *testing.T) {
	executor := New(WithoutSignalHandling())
	defer executor.Close()

	request, dir := helperRequest(t, EffectReadOnly, "spawn-child")
	pidPath := filepath.Join(dir, "child.pid")
	request.Args = append(request.Args, pidPath)
	request.IdleTimeout = 500 * time.Millisecond

	_, err := executor.Run(context.Background(), request)
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Run() error = %T %v, want *TimeoutError", err, err)
	}

	childPID := readPID(t, pidPath)
	waitForProcessGone(t, childPID)
}

func TestRunSignalKillsTrackedProcessGroup(t *testing.T) {
	signals := make(chan os.Signal, 1)
	executor := New(WithSignalChannel(signals))
	defer executor.Close()

	request, dir := helperRequest(t, EffectReadOnly, "spawn-child")
	pidPath := filepath.Join(dir, "child.pid")
	request.Args = append(request.Args, pidPath)
	request.IdleTimeout = 10 * time.Second

	type outcome struct {
		result RunResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := executor.Run(context.Background(), request)
		finished <- outcome{result: result, err: err}
	}()

	childPID := waitForPIDFile(t, pidPath)
	signals <- os.Interrupt

	select {
	case got := <-finished:
		var interruptedErr *InterruptedError
		if !errors.As(got.err, &interruptedErr) {
			t.Fatalf("Run() error = %T %v, want *InterruptedError", got.err, got.err)
		}
		if got.result.TimedOut {
			t.Fatalf("Run() result = %+v, want signal interruption", got.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after signal")
	}

	waitForProcessGone(t, childPID)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("invalid pid %q: %v", content, err)
	}
	return pid
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
			if parseErr == nil {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %q was not created", path)
	return 0
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("probe process %d: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after process-group kill", pid)
}
