//go:build !unix

package server

import "errors"

func execSelf(exePath string, args []string, env []string) error {
	return errors.New("in-place exec restart is only supported on Unix platforms")
}
