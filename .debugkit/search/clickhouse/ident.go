package clickhouse

import (
	"fmt"
	"strings"
)

func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func quote(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func checkIdentifier(kind, value string) error {
	if !validIdentifier(value) {
		return fmt.Errorf("clickhouse: invalid %s %q", kind, value)
	}
	return nil
}
