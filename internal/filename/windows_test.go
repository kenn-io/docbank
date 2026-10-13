package filename

import "testing"

func TestIsWindowsDeviceName(t *testing.T) {
	for _, name := range []string{"CON", "PRN", "AUX", "CONIN$.zip", "CONOUT$.zip", "com1.zip", "COM¹.zip", "COM².zip", "LPT³.zip", "LPT9.extra.zip", "NUL .jpg"} {
		if !IsWindowsDeviceName(name) {
			t.Errorf("IsWindowsDeviceName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"COM10.zip", "LPT0.zip"} {
		if IsWindowsDeviceName(name) {
			t.Errorf("IsWindowsDeviceName(%q) = true, want false", name)
		}
	}
}
