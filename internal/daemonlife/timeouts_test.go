package daemonlife

import "testing"

func TestClientGraceExceedsCompleteDaemonDrainBudget(t *testing.T) {
	if GracefulExitTimeout <= HTTPDrainTimeout+JobDrainTimeout {
		t.Fatalf("graceful exit wait %s must exceed daemon drain budget %s",
			GracefulExitTimeout, HTTPDrainTimeout+JobDrainTimeout)
	}
}
