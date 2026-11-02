package embedserver

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func openLibrary(path string) (uintptr, error) {
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	return uintptr(h), err
}
func closeLibrary(h uintptr) error { return windows.FreeLibrary(windows.Handle(h)) }
func librarySymbol(h uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(h), name)
}

func modelPath(path string) (unsafe.Pointer, error) {
	b, err := windows.UTF16FromString(path)
	if err != nil {
		return nil, err
	}
	return unsafe.Pointer(&b[0]), nil
}
