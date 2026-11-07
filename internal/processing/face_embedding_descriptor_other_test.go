//go:build !linux

package processing

import "testing"

func withFaceDescriptorBudget(t *testing.T, run func()) {
	t.Helper()
	run()
}
