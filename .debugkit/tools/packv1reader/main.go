// Command packv1reader intentionally uses only the buffered format-v1 pack API
// available in Kit v0.7.0. CI compiles this source in a temporary module pinned
// to that release and uses it to read a pack emitted by the current stream.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"go.kenn.io/kit/pack"
)

func readFixture(path string) error {
	reader, err := pack.OpenReader(path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	entries := reader.Entries()
	if len(entries) != 2 {
		return fmt.Errorf("got %d entries, want 2", len(entries))
	}
	var raw, compressed bool
	for _, entry := range entries {
		content, err := reader.ReadBlob(entry)
		if err != nil {
			return err
		}
		if pack.ComputeBlobID(content) != entry.ID {
			return fmt.Errorf("entry %s content identity mismatch", entry.ID)
		}
		if entry.Flags&pack.BlobCompressed != 0 {
			compressed = true
		} else {
			raw = true
		}
	}
	if !raw || !compressed {
		return errors.New("fixture does not contain both raw and compressed entries")
	}
	return nil
}

func main() {
	path := flag.String("pack", "", "format-v1 pack path")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "packv1reader: -pack is required")
		os.Exit(2)
	}
	if err := readFixture(*path); err != nil {
		fmt.Fprintln(os.Stderr, "packv1reader:", err)
		os.Exit(1)
	}
}
