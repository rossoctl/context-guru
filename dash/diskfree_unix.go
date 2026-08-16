//go:build unix

package dash

import "syscall"

// diskUsage reports the fraction of the filesystem holding path that is in use,
// counting only space a non-root process could actually use.
//
// Blocks-minus-Bavail rather than Blocks-minus-Bfree: filesystems reserve a slice
// for root, so Bfree overstates what this process can write and the watermark would
// fire late — exactly when late is useless.
//
// The fraction is of the WHOLE filesystem, not of this database. That is
// deliberate: the thing we are protecting against is the host running out of disk,
// and on a shared box most of the usage is somebody else's (container images, build
// trees, logs). A budget expressed only in our own bytes cannot see that coming.
func diskUsage(path string) (usedFrac float64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	// Bsize is int64 on Linux and uint32 on Darwin; convert both ways up.
	bsize := uint64(st.Bsize)
	total := uint64(st.Blocks) * bsize
	avail := uint64(st.Bavail) * bsize
	if total == 0 {
		return 0, false
	}
	if avail > total {
		return 0, false
	}
	return float64(total-avail) / float64(total), true
}
