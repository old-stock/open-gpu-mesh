//go:build windows

package assess

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// diskUsage returns total and free GB of the volume holding path.
func diskUsage(path string) (total, free int, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, totFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &tot, &totFree); err != nil {
		return 0, 0, err
	}
	return int(tot >> 30), int(avail >> 30), nil
}

// sameFS compares volume names (drive letters).
func sameFS(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}
