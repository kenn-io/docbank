package agenthook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

func planDirectJSONConfig(
	spec profileSpec,
	path, marker, command, commandWindows string,
	hooks []nativeHook,
	uninstall bool,
) ([]byte, bool, error) {
	root, exists, err := readJSONConfig(path)
	if err != nil {
		return nil, false, err
	}
	if spec.requireVersion {
		if version, ok := root["version"]; ok {
			if !directJSONVersionOne(version) {
				return nil, false, fmt.Errorf(
					"agent hook config %s version must be 1", path,
				)
			}
		}
	}
	before, err := marshalJSONConfig(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode existing agent hook config %s: %w", path, err)
	}

	hooksObject, err := jsonHooksObject(root, path, !uninstall)
	if err != nil {
		return nil, false, err
	}
	if hooksObject != nil {
		if err := removeOwnedDirectJSONHooks(hooksObject, marker, path); err != nil {
			return nil, false, err
		}
	}
	if !uninstall {
		if hooksObject == nil {
			return nil, false, fmt.Errorf("agent hook config %s has no hooks object", path)
		}
		if spec.requireVersion {
			if _, exists := root["version"]; !exists {
				root["version"] = 1
			}
		}
		for _, hook := range hooks {
			entry := map[string]any{"type": "command"}
			if slices.Contains(spec.failClosedEvents, hook.event) {
				entry["failClosed"] = true
			}
			if hook.matcher != "" {
				entry["matcher"] = hook.matcher
			}
			if hook.timeout > 0 {
				entry[spec.timeoutField] = hook.timeout
			}
			if spec.windowsCommandStyle == windowsCommandPowerShell && commandWindows != "" {
				entry["bash"] = command
				entry["powershell"] = commandWindows
			} else {
				entry["command"] = command
			}
			entries, err := jsonEventEntries(hooksObject, hook.name, path)
			if err != nil {
				return nil, false, err
			}
			hooksObject[hook.name] = append(entries, entry)
		}
	}
	after, err := marshalJSONConfig(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode agent hook config %s: %w", path, err)
	}
	changed := !bytes.Equal(before, after)
	if uninstall && !exists {
		changed = false
	}
	return after, changed, nil
}

func directJSONVersionOne(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, ok := new(big.Rat).SetString(number.String())
	return ok && parsed.Cmp(big.NewRat(1, 1)) == 0
}

func removeOwnedDirectJSONHooks(hooks map[string]any, marker, path string) error {
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("agent hook config %s event %q must be an array", path, event)
		}
		kept := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				kept = append(kept, rawEntry)
				continue
			}
			if directJSONEntryContains(entry, marker) {
				continue
			}
			kept = append(kept, rawEntry)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return nil
}

func directJSONEntryContains(entry map[string]any, marker string) bool {
	for _, field := range []string{"command", "bash", "powershell"} {
		command, _ := entry[field].(string)
		if strings.Contains(command, marker) {
			return true
		}
	}
	return false
}
