package brainservice

import (
	"errors"

	ole "github.com/go-ole/go-ole"
)

// go-ole returns a non-nil error for S_FALSE, although CoInitializeEx has
// successfully incremented the thread's COM initialization count. It must be
// balanced by CoUninitialize just like S_OK. This helper never calls COM.
func comInitializationResult(err error) error {
	var result *ole.OleError
	if errors.As(err, &result) && result.Code() == 1 {
		return nil
	}
	return err
}
