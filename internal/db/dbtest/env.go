//go:build postgres

package dbtest

import "os"

func lookupEnv(key string) string { return os.Getenv(key) }
