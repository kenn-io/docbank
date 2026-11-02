package packstore

import (
	"fmt"
	"os"
	"time"
)

// DebugTime prints one phase duration for the push stall investigation.
func DebugTime(phase string, start time.Time) {
	now := time.Now()
	fmt.Fprintf(os.Stderr, "pushdbg %s %s %.1fms\n", now.UTC().Format("15:04:05.000"), phase, float64(now.Sub(start).Microseconds())/1000)
}
