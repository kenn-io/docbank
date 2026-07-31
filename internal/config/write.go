package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/pack"
)

// EnsureStoreBindings makes the selected machine-local bindings available to
// a restored vault. An existing config is validated but never rewritten.
func EnsureStoreBindings(root string, bindings map[string]StoreBindingConfig) (retErr error) {
	if len(bindings) == 0 {
		return nil
	}
	path := filepath.Join(root, "config.toml")
	if _, err := os.Lstat(path); err == nil {
		current, loadErr := Load(root)
		if loadErr != nil {
			return loadErr
		}
		for name, expected := range bindings {
			actual, ok := current.StoreBindings[name]
			if !ok || actual != expected {
				return fmt.Errorf(
					"existing config.toml does not define restore binding %q exactly", name,
				)
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking restored config.toml: %w", err)
	}
	candidate := Default()
	candidate.StoreBindings = bindings
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("validating restored store bindings: %w", err)
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(struct {
		StoreBindings map[string]StoreBindingConfig `toml:"store_bindings"`
	}{StoreBindings: bindings}); err != nil {
		return fmt.Errorf("encoding restored store bindings: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating restored config.toml: %w", err)
	}
	remove := true
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, file.Close())
		}
		if remove {
			retErr = errors.Join(retErr, os.Remove(path))
		}
	}()
	if err := restrictWrittenConfig(path); err != nil {
		return err
	}
	if _, err := file.Write(encoded.Bytes()); err != nil {
		return fmt.Errorf("writing restored config.toml: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("syncing restored config.toml: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing restored config.toml: %w", err)
	}
	closed = true
	if err := pack.SyncDir(root); err != nil {
		return fmt.Errorf("syncing restored config directory: %w", err)
	}
	remove = false
	return nil
}
