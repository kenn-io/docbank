//go:build !windows

package config

func restrictWrittenConfig(string) error { return nil }
