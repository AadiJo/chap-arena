//go:build !windows

package update

import (
	"os"
	"syscall"
)

// Replaces this process with a fresh run of executable with args, keeping its PID, environment and terminal. Go opens
// files and sockets close-on-exec, so event.db's lock and the ports are free for the new run.
func Restart(executable string, args []string) error {
	return syscall.Exec(executable, append([]string{executable}, args...), os.Environ())
}
