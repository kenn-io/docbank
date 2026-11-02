//go:build windows

package safefileio

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

type tokenOwner struct {
	Owner *windows.SID
}

func currentWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func currentWindowsOwnerSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	n := uint32(64)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], uint32(len(buf)), &n)
		if err == nil {
			owner := (*tokenOwner)(unsafe.Pointer(&buf[0]))
			if owner.Owner == nil {
				return nil, errors.New("current token owner is missing")
			}
			return owner.Owner.Copy()
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
		if n <= uint32(len(buf)) {
			return nil, err
		}
	}
}

func windowsOwnerMatches(owner *windows.SID, allowed ...*windows.SID) bool {
	if owner == nil {
		return false
	}
	for _, sid := range allowed {
		if sid != nil && owner.Equals(sid) {
			return true
		}
	}
	return false
}
