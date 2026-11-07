package brainservice

import (
	"context"
	"errors"
	"runtime"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// taskState uses only the current user's local scheduler. Callers in tests
// inject this boundary; development tests never instantiate or call COM.
func taskState(ctx context.Context, id string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := comInitializationResult(ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)); err != nil {
		return 0, err
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		return 0, err
	}
	defer unknown.Release()
	service, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return 0, err
	}
	defer service.Release()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	connected, err := oleutil.CallMethod(service, "Connect")
	if err != nil {
		return 0, err
	}
	_ = connected.Clear()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	folderValue, err := oleutil.CallMethod(service, "GetFolder", `\`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = folderValue.Clear() }()
	folder := folderValue.ToIDispatch()
	if folder == nil {
		return 0, ErrManager
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	taskValue, err := oleutil.CallMethod(folder, "GetTask", id)
	if err != nil {
		var comError *ole.OleError
		if errors.As(err, &comError) && uint32(comError.Code()) == 0x80070002 {
			return 0, ErrNotInstalled
		}
		return 0, err
	}
	defer func() { _ = taskValue.Clear() }()
	task := taskValue.ToIDispatch()
	if task == nil {
		return 0, ErrManager
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stateValue, err := oleutil.GetProperty(task, "State")
	if err != nil {
		return 0, err
	}
	defer func() { _ = stateValue.Clear() }()
	if stateValue.VT != ole.VT_I4 && stateValue.VT != ole.VT_I2 && stateValue.VT != ole.VT_UI4 {
		return 0, ErrManager
	}
	return int(stateValue.Val), ctx.Err()
}
