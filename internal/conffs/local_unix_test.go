//go:build unix

package conffs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO named like an editable file is refused without being opened, so
// neither a read nor a write (which holds the lock) nor a search blocks.
func TestLocalFIFO(t *testing.T) {
	f := newFixture(t)
	if err := syscall.Mkfifo(filepath.Join(f.root, "fifo.xml"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Symlink("fifo.xml", filepath.Join(f.root, "fifolink.xml"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range []string{"fifo.xml", "fifolink.xml"} {
			if _, err := f.l.Read(ctx, p); !isInvalid(err) {
				t.Errorf("read %s: %v", p, err)
			}
		}
		if _, err := f.l.Write(ctx, "fifo.xml", "<x/>", "", "u"); !isInvalid(err) {
			t.Errorf("write: %v", err)
		}
		res, err := f.l.Search(ctx, "X-PRE-PROCESS", SearchOptions{})
		if err != nil || len(res.Rows) != 1 || res.Rows[0].Path != "vars.xml" {
			t.Errorf("search: %+v %v", res, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a FIFO")
	}
}
