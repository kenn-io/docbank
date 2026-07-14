// Package daemonlife defines lifecycle budgets shared by the daemon and the
// clients that wait for it to stop.
package daemonlife

import "time"

const (
	HTTPDrainTimeout = 10 * time.Second
	JobDrainTimeout  = 10 * time.Second

	// GracefulExitTimeout must exceed the daemon's consecutive HTTP and job
	// drain windows. The margin covers scheduling and cleanup defers before the
	// process exits; clients must not force termination inside this window.
	GracefulExitTimeout = HTTPDrainTimeout + JobDrainTimeout + 5*time.Second
	ForcedExitTimeout   = 10 * time.Second
)
