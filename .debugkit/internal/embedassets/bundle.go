package embedassets

import "runtime"

// BundleTag selects a published server with the pinned model and runtime.
// Publish all six bundles before changing this tag.
const BundleTag = "v0.47.1"

// ExecutableName is the bundle's platform executable.
func ExecutableName() string {
	if runtime.GOOS == "windows" {
		return "kennembed.exe"
	}
	return "kennembed"
}

// LibraryName is the runtime loaded beside the server.
func LibraryName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.1.23.2.dylib"
	default:
		return "libonnxruntime.so.1.23.2"
	}
}

// NativeFiles lists required libraries, including Windows runtime dependencies.
func NativeFiles() []string {
	names := []string{LibraryName()}
	if runtime.GOOS == "windows" {
		names = append(names, "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll", "vcruntime140_1.dll")
	}
	return names
}
