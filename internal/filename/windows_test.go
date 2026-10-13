package filename

import "testing"

func TestIsWindowsDeviceName(t *testing.T) {
	for _, name := range []string{"CONIN$.zip", "CONOUT$.zip", "com1.zip", "COM¹.zip", "COM².zip", "LPT³.zip", "LPT9.extra.zip", "NUL .jpg"} {
		if !IsWindowsDeviceName(name) {
			t.Errorf("device name %q was accepted", name)
		}
	}
	for _, name := range []string{"COM10.zip", "LPT0.zip"} {
		if IsWindowsDeviceName(name) {
			t.Errorf("ordinary name %q was rejected", name)
		}
	}
}
