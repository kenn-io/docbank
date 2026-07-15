package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

const putProgressByteInterval = 4 << 20

var (
	putJSON     bool
	putProgress string
	putMIMEType string
)

var putCmd = &cobra.Command{
	Use:   "put <source-file> <vault-path>",
	Short: "Replace a file's content while retaining its immutable history",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source, sourcePath, size, err := openPutSource(args[0])
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()

		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		target, err := c.Stat(cmd.Context(), args[1])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[1], err)
		}
		if target.Kind != "file" {
			return fmt.Errorf("%q: %w", args[1], store.ErrNotFile)
		}

		mimeType, err := putSourceMIME(source, sourcePath, putMIMEType)
		if err != nil {
			return err
		}
		mode := backupProgressAuto
		if !putJSON {
			mode, err = progressModeFromFlag("put", putProgress)
			if err != nil {
				return err
			}
		}
		renderer := newBackupProgressRenderer(cmd.ErrOrStderr(), mode)
		defer renderer.finish()

		hash := sha256.New()
		hashReader := newPutProgressReader(cmd.Context(), source, size, "hash", renderer, putJSON)
		read, err := io.Copy(hash, hashReader)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", sourcePath, err)
		}
		if read != size {
			return fmt.Errorf("hashing %s: source size changed from %d to %d bytes", sourcePath, size, read)
		}
		hashReader.finish()
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding %s: %w", sourcePath, err)
		}

		uploadReader := newPutProgressReader(cmd.Context(), source, size, "upload", renderer, putJSON)
		receipt, err := c.ReplaceContent(cmd.Context(), target.ID, target.Revision, mimeType,
			hex.EncodeToString(hash.Sum(nil)), size, uploadReader)
		if err != nil {
			return fmt.Errorf("replacing %q: %w", args[1], err)
		}
		uploadReader.finish()
		if putJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			if err := enc.Encode(receipt); err != nil {
				return fmt.Errorf("writing replacement receipt: %w", err)
			}
			return nil
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"updated %s to version %s (revision %d, %s, sha256 %s)\n",
			args[1], receipt.Version.ID, receipt.Node.Revision,
			formatBackupBytes(receipt.ComputedSize), receipt.ComputedHash)
		if err != nil {
			return fmt.Errorf("writing replacement receipt: %w", err)
		}
		return nil
	},
}

func openPutSource(raw string) (*os.File, string, int64, error) {
	if !utf8.ValidString(raw) {
		return nil, "", 0, fmt.Errorf("replacement source path %s is not valid UTF-8",
			strconv.QuoteToASCII(raw))
	}
	path, err := filepath.Abs(raw)
	if err != nil {
		return nil, "", 0, fmt.Errorf("resolving %q: %w", raw, err)
	}
	file, err := blob.OpenNoFollow(path)
	if err != nil {
		return nil, "", 0, fmt.Errorf("opening %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", 0, fmt.Errorf("checking %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", 0, fmt.Errorf("%s: not a regular file", path)
	}
	if info.Size() > blob.MaxIngestBytes {
		_ = file.Close()
		return nil, "", 0, fmt.Errorf("%s is %d bytes; maximum replacement size is %d",
			path, info.Size(), blob.MaxIngestBytes)
	}
	return file, path, info.Size(), nil
}

func putSourceMIME(source io.ReadSeeker, path, override string) (string, error) {
	if override != "" {
		mediaType, params, err := mime.ParseMediaType(override)
		if err != nil {
			return "", fmt.Errorf("invalid --mime-type %q: %w", override, err)
		}
		return mime.FormatMediaType(mediaType, params), nil
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(source, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("reading %s for media type: %w", path, err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding %s after media-type detection: %w", path, err)
	}
	if byExtension := mime.TypeByExtension(filepath.Ext(path)); byExtension != "" {
		return byExtension, nil
	}
	return http.DetectContentType(head[:n]), nil
}

type putProgressReader struct {
	ctx          context.Context
	reader       io.Reader
	total        int64
	stage        string
	renderer     *backupProgressRenderer
	suppressed   bool
	done         int64
	lastRendered int64
	lastAt       time.Time
}

func newPutProgressReader(
	ctx context.Context, reader io.Reader, total int64, stage string,
	renderer *backupProgressRenderer, suppressed bool,
) *putProgressReader {
	p := &putProgressReader{ctx: ctx, reader: reader, total: total, stage: stage,
		renderer: renderer, suppressed: suppressed}
	p.render(false)
	return p
}

func (p *putProgressReader) Read(buf []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.reader.Read(buf)
	p.done += int64(n)
	if !p.suppressed && (p.done-p.lastRendered >= putProgressByteInterval ||
		(!p.lastAt.IsZero() && time.Since(p.lastAt) >= time.Second)) {
		p.render(false)
	}
	if err == nil {
		err = p.ctx.Err()
	}
	return n, err
}

func (p *putProgressReader) finish() { p.render(true) }

func (p *putProgressReader) render(final bool) {
	if p.suppressed {
		return
	}
	done := int64(0)
	if final {
		done = 1
	}
	p.renderer.handle(api.BackupProgress{
		Stage: p.stage, Done: done, Total: 1,
		BytesDone: p.done, BytesTotal: p.total, Final: final,
	})
	p.lastRendered = p.done
	p.lastAt = time.Now()
}

func init() {
	putCmd.Flags().BoolVar(&putJSON, "json", false,
		"emit a machine-readable replacement receipt (progress suppressed)")
	putCmd.Flags().StringVar(&putProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	putCmd.Flags().StringVar(&putMIMEType, "mime-type", "",
		"override the media type detected from the source file")
	rootCmd.AddCommand(putCmd)
}
