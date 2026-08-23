package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
)

func writeCLIJSON(w io.Writer, value any) error {
	if err := json.MarshalEncode(jsontext.NewEncoder(w), value); err != nil {
		return fmt.Errorf("writing JSON output: %w", err)
	}
	return nil
}
