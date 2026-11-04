//go:build !windows

package assess

import "syscall"

// diskUsage returns total and free (available to unprivileged users) GB of the filesystem at path.
func diskUsage(path string) (total, free int, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return int(st.Blocks * bs >> 30), int(st.Bavail * bs >> 30), nil
}

// sameFS reports whether two paths are on the same filesystem.
func sameFS(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return a == b
	}
	return sa.Dev == sb.Dev
}
