//go:build windows

package cli

import "errors"

func reexec(string) error {
	return errors.New("in-place upgrade is not supported on Windows; restart the agent")
}
