package a

import "time"

// Production code may sleep; the analyzer only inspects test files.
func Backoff() {
	time.Sleep(time.Millisecond)
}
