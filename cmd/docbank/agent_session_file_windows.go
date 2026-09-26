//go:build windows

package main

import "go.kenn.io/docbank/internal/winsecurity"

func secureAgentSessionStagedFile(path string) error {
	return winsecurity.RestrictCurrentUserFile(path)
}
