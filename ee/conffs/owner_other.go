//go:build !community && !unix

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package conffs

import (
	"io/fs"
	"os"
)

func keepOwner(*os.File, fs.FileInfo) {}
