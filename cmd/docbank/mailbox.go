package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func init() { rootCmd.AddCommand(newMailboxCommand()) }
func newMailboxCommand() *cobra.Command {
	root := &cobra.Command{Use: "mailbox", Short: "Import verified MBOX, Takeout ZIP, or explicitly identified EML"}
	var dest, dialect, id string
	var preview bool
	var labelTags map[string]string
	upload := &cobra.Command{Use: "import <mbox-or-zip>", Short: "Stream an archive to the daemon and start a resumable import", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		dir, err := c.Stat(cmd.Context(), dest)
		if err != nil {
			return err
		}
		file, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > store.MailboxContainerBytes {
			return store.ErrMailboxLimit
		}
		hash, err := hashMailboxSource(cmd.Context(), file, info.Size())
		if err != nil {
			return err
		}
		if id == "" {
			id = rand.Text()
		}
		format := "mbox"
		if strings.EqualFold(filepath.Ext(args[0]), ".zip") {
			format = "zip"
		}
		request := store.MailboxContainerRequest{ID: id, Format: format, SHA256: hash, Size: info.Size()}
		container, err := c.BeginMailboxContainer(cmd.Context(), request)
		if err != nil {
			return err
		}
		for index, offset := 0, int64(0); offset < info.Size() && container.State != "sealed"; index, offset = index+1, offset+store.MailboxChunkBytes {
			size := min(store.MailboxChunkBytes, info.Size()-offset)
			chunkHash, err := hashMailboxSource(cmd.Context(), io.NewSectionReader(file, offset, size), size)
			if err != nil {
				return err
			}
			if err = c.UploadMailboxChunk(cmd.Context(), id, index, chunkHash, size, io.NewSectionReader(file, offset, size)); err != nil {
				return fmt.Errorf("upload interrupted; retry with --id %s: %w", id, err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Uploaded %d of %d bytes\n", offset+size, info.Size())
		}
		if _, err = c.SealMailboxContainer(cmd.Context(), id); err != nil {
			return err
		}
		sample, err := c.PreviewMailbox(cmd.Context(), id, dialect)
		if err != nil {
			return err
		}
		if preview {
			return json.MarshalWrite(cmd.OutOrStdout(), struct {
				ContainerID string `json:"container_id"`
				SHA256      string `json:"sha256"`
				Preview     any    `json:"preview"`
			}{id, hash, sample})
		}
		job, err := c.BeginMailboxJob(cmd.Context(), store.MailboxJobRequest{ID: id, ContainerID: id, ContainerSHA256: hash, Settings: store.MailboxSettings{Dialect: dialect, DestinationID: dir.ID, LabelTags: labelTags}})
		if err != nil {
			return err
		}
		return json.MarshalWrite(cmd.OutOrStdout(), job)
	}}
	upload.Flags().StringVar(&dest, "dest", "/", "Destination virtual directory")
	upload.Flags().StringVar(&dialect, "dialect", "mboxrd", "Explicit mailbox dialect: mboxrd or mboxo")
	upload.Flags().StringVar(&id, "id", "", "Stable upload/import identity for retries")
	upload.Flags().BoolVar(&preview, "preview", false, "Retain the verified container and print a preview without importing messages")
	upload.Flags().StringToStringVar(&labelTags, "label-tag", nil, "Explicit source-label=existing-tag-ID mapping (repeatable)")
	root.AddCommand(upload)
	for _, action := range []string{"status", "watch", "resume", "continue", "cancel", "receipts"} {
		var after int64
		var limit int
		command := &cobra.Command{Use: action + " <job-id>", Short: strings.ToUpper(action[:1]) + action[1:] + " a mailbox import", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if action == "cancel" {
				return c.CancelMailboxJob(cmd.Context(), args[0])
			}
			if action == "watch" {
				return c.WatchMailboxJob(cmd.Context(), args[0], func(j store.MailboxJob) error {
					if err := json.MarshalWrite(cmd.OutOrStdout(), j); err != nil {
						return err
					}
					_, err := fmt.Fprintln(cmd.OutOrStdout())
					if err != nil {
						return fmt.Errorf("write mailbox event: %w", err)
					}
					return nil
				})
			}
			if action == "receipts" {
				out, err := c.MailboxOccurrences(cmd.Context(), args[0], after, limit)
				if err != nil {
					return err
				}
				return json.MarshalWrite(cmd.OutOrStdout(), out)
			}
			job, err := c.MailboxJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if action == "resume" || action == "continue" {
				job, err = c.ResumeMailboxJob(cmd.Context(), job.MailboxJobRequest, action == "continue")
				if err != nil {
					return err
				}
			}
			return json.MarshalWrite(cmd.OutOrStdout(), job)
		}}
		if action == "receipts" {
			command.Flags().Int64Var(&after, "after", 0, "Return occurrences after this ordinal")
			command.Flags().IntVar(&limit, "limit", 100, "Page size, at most 100")
		}
		root.AddCommand(command)
	}
	root.AddCommand(newMailboxArchiveCommand(), newMailboxTransferCommand())
	return root
}

func hashMailboxSource(ctx context.Context, source io.Reader, size int64) (string, error) {
	if size < 1 || size > store.MailboxContainerBytes {
		return "", store.ErrMailboxLimit
	}
	h := sha256.New()
	reader := newPutProgressReader(ctx, io.LimitReader(source, size+1), size, "hashing", nil, true)
	n, err := io.CopyBuffer(h, reader, make([]byte, 64<<10))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", store.ErrMailboxConflict
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
