// Package herdr launches the local Herdr client attached to the remote workbox
// over OpenSSH. Herdr keeps its server, panes and sessions on the remote host,
// so running `herdr --remote <host>` gives a local UI over a persistent remote
// session (see https://herdr.dev/docs/persistence-remote/).
package herdr

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Binary is the herdr executable name, overridable for tests.
var Binary = "herdr"

// remoteArgs builds the argument list to attach to an SSH target.
func remoteArgs(sshTarget string) []string {
	return []string{"--remote", sshTarget}
}

// Exec replaces the current process with the local herdr client attached to the
// SSH target. It only returns on failure to start.
func Exec(sshTarget string) error {
	path, err := exec.LookPath(Binary)
	if err != nil {
		return fmt.Errorf("herdr not found on PATH: install it from https://herdr.dev/docs/install/ (%w)", err)
	}
	argv := append([]string{Binary}, remoteArgs(sshTarget)...)
	return syscall.Exec(path, argv, os.Environ())
}
