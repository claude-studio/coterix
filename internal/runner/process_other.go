//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runner

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}

func killProcessTree(process *os.Process) error {
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
