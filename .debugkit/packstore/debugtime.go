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

// The heartbeat tells a blocked file system from a frozen process: its ticks
// stay on time while only file calls block.
func init() {
	if os.Getenv("PUSHDBG_HEARTBEAT") == "" {
		return
	}
	go func() {
		const period = 100 * time.Millisecond
		last := time.Now()
		for range time.Tick(period) {
			now := time.Now()
			if late := now.Sub(last) - period; late >= 200*time.Millisecond {
				fmt.Fprintf(os.Stderr, "pushdbg %s heartbeat.late %.1fms\n", now.UTC().Format("15:04:05.000"), float64(late.Microseconds())/1000)
			}
			last = now
		}
	}()
}
