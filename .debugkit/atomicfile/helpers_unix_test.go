//go:build unix

package atomicfile_test

func symlinkPrivilegeMissing(error) bool { return false }
