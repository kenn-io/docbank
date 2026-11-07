package brainservice

import "testing"

func TestBrainServiceNativeDriver(t *testing.T) {
	nativeArgvChild(t)
	TestBrainServiceOwnedLifecycle(t)
}
