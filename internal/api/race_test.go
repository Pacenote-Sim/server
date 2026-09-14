//go:build postgres && race

package api_test

// raceEnabled is true in a build with the race detector on.
//
// It gates the two timing assertions in this package. Every read and write in
// an instrumented build goes through the detector's bookkeeping, which makes a
// wall-clock measurement a measurement of the detector; the benchmarks are the
// real number, and they are run without it.
const raceEnabled = true
