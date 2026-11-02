// Command packstreamfixture writes a small format-v1 pack through the current
// streaming API for compatibility checks against older readers.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
)

type noiseReader struct{ state uint32 }

func (r *noiseReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state << 13
		r.state ^= r.state >> 17
		r.state ^= r.state << 5
		p[i] = byte(r.state)
	}
	return len(p), nil
}

func writeFixture(output string) error {
	if output == "" {
		return errors.New("output path is required")
	}
	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	if err != nil {
		return err
	}
	abort := true
	defer func() {
		if abort {
			_ = writer.Abort()
		}
	}()
	const rawSize = uint64(64 << 10)
	if _, err := writer.AppendStream(context.Background(),
		io.LimitReader(&noiseReader{state: 1}, int64(rawSize)), rawSize,
		pack.AppendStreamOptions{ScratchDir: dir}); err != nil {
		return err
	}
	compressed := bytes.Repeat([]byte("format-v1 compatibility "), 4096)
	if _, err := writer.AppendStream(context.Background(), bytes.NewReader(compressed), uint64(len(compressed)),
		pack.AppendStreamOptions{ScratchDir: dir}); err != nil {
		return err
	}
	if _, err := writer.Seal(output); err != nil {
		return err
	}
	abort = false
	return nil
}

func main() {
	output := flag.String("out", "", "output pack path")
	flag.Parse()
	if err := writeFixture(*output); err != nil {
		fmt.Fprintln(os.Stderr, "packstreamfixture:", err)
		os.Exit(1)
	}
}
