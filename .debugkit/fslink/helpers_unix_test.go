//go:build unix

package fslink_test

func symlinkPrivilegeMissing(error) bool { return false }

func platformLinkMakers() []linkMaker { return nil }
