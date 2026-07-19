package main

import (
	"encoding/json"
	"fmt"
	"io"
)

func writeCLIJSON(w io.Writer, value any) error {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return fmt.Errorf("writing JSON output: %w", err)
	}
	return nil
}
