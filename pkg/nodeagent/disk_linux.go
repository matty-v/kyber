package nodeagent

import "syscall"

// getDiskStats returns (used, total) bytes for the filesystem at mountPoint.
// Uses syscall.Statfs which is available on Linux (the DaemonSet target).
func getDiskStats(mountPoint string) (used, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountPoint, &stat); err != nil {
		return 0, 0, err
	}
	total = stat.Blocks * uint64(stat.Bsize) //nolint:unconvert
	avail := stat.Bavail * uint64(stat.Bsize) //nolint:unconvert
	used = total - avail
	return used, total, nil
}
