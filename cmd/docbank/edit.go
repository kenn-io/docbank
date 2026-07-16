package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/shlex"
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var (
	editEditor   string
	editMIMEType string
	editProgress string
)

var editCmd = &cobra.Command{
	Use:   "edit <vault-path>",
	Short: "Edit a file through a new immutable content version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEdit(cmd, args[0])
	},
}

func runEdit(cmd *cobra.Command, vaultPath string) (retErr error) {
	editor, err := editorCommand(editEditor)
	if err != nil {
		return err
	}
	mimeOverride, err := normalizedMIMEOverride(editMIMEType)
	if err != nil {
		return err
	}
	progressMode, err := progressModeFromFlag("edit", editProgress)
	if err != nil {
		return err
	}
	renderer := newBackupProgressRenderer(cmd.ErrOrStderr(), progressMode)
	defer renderer.finish()

	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	node, err := c.Stat(cmd.Context(), vaultPath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", vaultPath, err)
	}
	if node.Kind != "file" {
		return fmt.Errorf("%q: %w", vaultPath, store.ErrNotFile)
	}

	stageDir, err := os.MkdirTemp("", "docbank-edit-*")
	if err != nil {
		return fmt.Errorf("creating private edit staging: %w", err)
	}
	stageRemoved := false
	defer func() {
		if stageRemoved {
			return
		}
		if cleanupErr := os.RemoveAll(stageDir); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("removing private edit staging: %w", cleanupErr))
		}
	}()
	stagePath, err := stageCurrentVersion(cmd.Context(), c, node, stageDir, renderer)
	if err != nil {
		return fmt.Errorf("staging %q: %w", vaultPath, err)
	}

	// The editor command is deliberately selected by the local operator through
	// a flag or environment; no remote or vault content reaches the executable.
	process := exec.CommandContext( //nolint:gosec // executing the user's configured editor is this command's purpose
		cmd.Context(), editor[0], append(editor[1:], stagePath)...,
	)
	process.Stdin = cmd.InOrStdin()
	process.Stdout = cmd.OutOrStdout()
	process.Stderr = cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("editor %q failed: %w", editor[0], err)
	}

	edited, editedPath, editedSize, err := openPutSource(stagePath)
	if err != nil {
		return fmt.Errorf("opening edited content: %w", err)
	}
	editedOpen := true
	defer func() {
		if editedOpen {
			_ = edited.Close()
		}
	}()
	hash := sha256.New()
	hashReader := newPutProgressReader(
		cmd.Context(), edited, editedSize, "hash", renderer, false,
	)
	read, err := io.Copy(hash, hashReader)
	if err != nil {
		return fmt.Errorf("hashing edited content: %w", err)
	}
	if read != editedSize {
		return fmt.Errorf("hashing edited content: size changed from %d to %d bytes", editedSize, read)
	}
	hashReader.finish()
	editedHash := hex.EncodeToString(hash.Sum(nil))
	editedMIME := node.MimeType
	if mimeOverride != "" {
		editedMIME = mimeOverride
	}
	if editedHash == node.BlobHash && editedSize == node.Size && editedMIME == node.MimeType {
		if err := edited.Close(); err != nil {
			return fmt.Errorf("closing unchanged edit: %w", err)
		}
		editedOpen = false
		if err := os.RemoveAll(stageDir); err != nil {
			return fmt.Errorf("removing unchanged edit staging: %w", err)
		}
		stageRemoved = true
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "unchanged %s (version %s)\n",
			vaultPath, node.CurrentVersionID); err != nil {
			return fmt.Errorf("writing unchanged edit result: %w", err)
		}
		return nil
	}
	if editedMIME == "" {
		editedMIME = "application/octet-stream"
	}
	if _, err := edited.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding %s: %w", editedPath, err)
	}

	// Editors may remain open much longer than the daemon idle timeout. Reacquire
	// a compatible daemon only after local hashing, while retaining the original
	// node revision so a concurrent mutation fails instead of being overwritten.
	c, err = client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	uploadReader := newPutProgressReader(
		cmd.Context(), edited, editedSize, "upload", renderer, false,
	)
	receipt, err := c.ReplaceContent(cmd.Context(), node.ID, node.Revision, editedMIME,
		editedHash, editedSize, uploadReader)
	if err != nil {
		return fmt.Errorf("saving edits to %q: %w", vaultPath, err)
	}
	uploadReader.finish()
	_ = edited.Close()
	editedOpen = false
	cleanupErr := os.RemoveAll(stageDir)
	stageRemoved = true
	if cleanupErr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: content was updated, but private edit staging remains at %s: %v\n",
			stageDir, cleanupErr)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"updated %s to version %s (revision %d, %s, sha256 %s)\n",
		vaultPath, receipt.Version.ID, receipt.Node.Revision,
		formatBackupBytes(receipt.ComputedSize), receipt.ComputedHash)
	if err != nil {
		return fmt.Errorf("writing edit result: %w", err)
	}
	return nil
}

func editorCommand(override string) ([]string, error) {
	raw := strings.TrimSpace(override)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("VISUAL"))
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if raw == "" {
		if runtime.GOOS == "windows" {
			raw = "notepad.exe"
		} else {
			raw = "vi"
		}
	}
	parts, err := shlex.Split(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing editor command %q: %w", raw, err)
	}
	if len(parts) == 0 {
		return nil, errors.New("editor command is empty")
	}
	return parts, nil
}

func normalizedMIMEOverride(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("invalid --mime-type %q: %w", value, err)
	}
	return mime.FormatMediaType(mediaType, params), nil
}

func stageCurrentVersion(
	ctx context.Context, c *client.Client, node api.Node, stageDir string,
	renderer *backupProgressRenderer,
) (path string, retErr error) {
	stream, err := c.VersionContent(ctx, node.CurrentVersionID)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing content stream: %w", closeErr))
		}
	}()
	if stream.BlobHash != node.BlobHash || stream.Size != node.Size {
		return "", fmt.Errorf("version authority %s/%d disagrees with node authority %s/%d",
			stream.BlobHash, stream.Size, node.BlobHash, node.Size)
	}

	file, err := os.CreateTemp(stageDir, "document-*"+filepath.Ext(node.Name))
	if err != nil {
		return "", fmt.Errorf("creating staged file: %w", err)
	}
	path = file.Name()
	defer func() {
		if file != nil {
			if closeErr := file.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("closing staged file: %w", closeErr))
			}
		}
	}()
	progress := newEditProgressWriter(ctx, file, node.Size, renderer)
	if _, err := stream.CopyVerified(progress); err != nil {
		return "", err
	}
	progress.finish()
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("syncing staged file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing staged file before editor: %w", err)
	}
	file = nil
	return path, nil
}

type editProgressWriter struct {
	ctx          context.Context
	writer       io.Writer
	total        int64
	renderer     *backupProgressRenderer
	done         int64
	lastRendered int64
	lastAt       time.Time
}

func newEditProgressWriter(
	ctx context.Context, writer io.Writer, total int64, renderer *backupProgressRenderer,
) *editProgressWriter {
	w := &editProgressWriter{ctx: ctx, writer: writer, total: total, renderer: renderer}
	w.render(false)
	return w
}

func (w *editProgressWriter) Write(buf []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(buf)
	w.done += int64(n)
	if w.done-w.lastRendered >= putProgressByteInterval ||
		(!w.lastAt.IsZero() && time.Since(w.lastAt) >= time.Second) {
		w.render(false)
	}
	if err == nil {
		err = w.ctx.Err()
	}
	return n, err
}

func (w *editProgressWriter) finish() { w.render(true) }

func (w *editProgressWriter) render(final bool) {
	done := int64(0)
	if final {
		done = 1
	}
	w.renderer.handle(api.BackupProgress{
		Stage: "download", Done: done, Total: 1,
		BytesDone: w.done, BytesTotal: w.total, Final: final,
	})
	w.lastRendered = w.done
	w.lastAt = time.Now()
}

func init() {
	editCmd.Flags().StringVar(&editEditor, "editor", "",
		"blocking editor command (default: VISUAL, EDITOR, then platform editor)")
	editCmd.Flags().StringVar(&editMIMEType, "mime-type", "",
		"set the media type on the new version instead of preserving the current type")
	editCmd.Flags().StringVar(&editProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain")
	rootCmd.AddCommand(editCmd)
}
