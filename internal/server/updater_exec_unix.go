//go:build unix

package server

import "syscall"

// execSelf replaces the current process image in place with exePath, preserving
// the PID so systemd (both Restart=on-failure and Restart=always), launchd, and
// foreground sessions stay attached cleanly after a self-update.
func execSelf(exePath string, args []string, env []string) error {
	return syscall.Exec(exePath, args, env)
}
