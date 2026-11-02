//go:build !windows

package daemon

import "go.kenn.io/kit/safefileio"

func runtimeUID() string {
	id, err := safefileio.CurrentUserID()
	if err != nil {
		return "unknown"
	}
	return id
}
