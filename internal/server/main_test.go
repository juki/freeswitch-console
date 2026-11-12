package server

import (
	"os"
	"testing"
)

// defaultIterations is read before TestMain lowers the count.
var defaultIterations = pbkdf2Iterations

func TestMain(m *testing.M) {
	// Production hashes cost 600000 iterations; the tests only need the
	// format and the comparison.
	pbkdf2Iterations = 1000
	os.Exit(m.Run())
}
