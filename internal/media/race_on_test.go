//go:build race

package media

// raceEnabled skips the heap measurements, which the race detector distorts.
const raceEnabled = true
