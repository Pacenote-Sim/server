//go:build postgres && !race

package api_test

// raceEnabled is false in an ordinary build. See race_test.go.
const raceEnabled = false
