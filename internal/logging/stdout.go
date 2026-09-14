package logging

import (
	"io"
	"os"
)

// stdout is a function rather than a package variable so that a test can never
// accidentally share one writer with the process's real output.
func stdout() io.Writer { return os.Stdout }
