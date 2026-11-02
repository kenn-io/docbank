package renamebusy

import (
	"errors"

	"golang.org/x/sys/windows"
)

// fileBusy detects handles that block a Windows rename because they omit
// FILE_SHARE_DELETE.
func fileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
