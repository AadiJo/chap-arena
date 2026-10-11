package update

import (
	"os"
	"os/exec"
)

// Starts executable with args in this console window, then exits. Windows can't replace a running process, so for a
// moment both run: the new one waits up to a second for this one to release event.db (see model.OpenDatabase) before
// it binds the ports, and this one exits right after starting it.
func Restart(executable string, args []string) error {
	command := exec.Command(executable, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
