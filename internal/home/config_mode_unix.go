//go:build !windows

package home

func secureOptionalConfig(string) error {
	return nil
}
