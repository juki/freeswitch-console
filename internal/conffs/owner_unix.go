//go:build unix

package conffs

import (
	"io/fs"
	"os"
	"syscall"
)

// keepOwner gives f the owner of the file it replaces, so FreeSWITCH can
// still read a file it owned. It only works for root or the same owner and
// is skipped silently otherwise.
func keepOwner(f *os.File, old fs.FileInfo) {
	if st, ok := old.Sys().(*syscall.Stat_t); ok {
		f.Chown(int(st.Uid), int(st.Gid))
	}
}
