//go:build !unix

package conffs

import (
	"io/fs"
	"os"
)

func keepOwner(*os.File, fs.FileInfo) {}
