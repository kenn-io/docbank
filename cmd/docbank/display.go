package main

import (
	"fmt"
	"time"
)

func formatHumanTimestamp(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("formatting timestamp %q: %w", value, err)
	}
	return parsed.UTC().Format(time.RFC3339), nil
}
