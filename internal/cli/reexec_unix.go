//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func reexec(exe string) error {
	return syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), os.Environ())
}
