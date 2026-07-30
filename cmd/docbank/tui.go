package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	doctui "go.kenn.io/docbank/internal/tui"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Browse and search the vault interactively",
	Long: `Open a terminal interface backed by the authenticated daemon API.

Navigation:
  Up/Down or j/k       Move between documents
  Enter or Right       Open a directory
  Enter on a file or i Inspect complete document authority
  x                    Move the selected node to recoverable trash
  T                    Browse and restore recoverable trash
  a                    Browse permanent audited history
  O                    Inspect storage and backup state
  Left or Backspace    Return to the parent directory
  /                    Search names and extracted text
  s                    Cycle the sort column
  v                    Reverse the sort direction
  r                    Refresh the current view
  ?                    Show keyboard help
  q                    Quit

Trash and restore require an explicit revision-bound confirmation. Permanent
deletion, storage maintenance, backup creation/restore, and permanent-audit
enrollment remain outside the TUI.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		backend := &tuiDaemonBackend{ensure: client.Ensure, initial: c}
		defer func() { _ = backend.Close() }()
		model, err := doctui.New(cmd.Context(), backend)
		if err != nil {
			return err
		}
		if _, err := tea.NewProgram(model).Run(); err != nil {
			return fmt.Errorf("running docbank TUI: %w", err)
		}
		return nil
	},
}

func (b *tuiDaemonBackend) Close() error {
	b.mu.Lock()
	c := b.initial
	b.initial = nil
	b.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

type tuiClientFactory func(context.Context) (*client.Client, error)

// tuiDaemonBackend reacquires the daemon around each bounded interaction. A
// TUI can remain open longer than the configured daemon idle timeout, and a
// proven client intentionally refuses to reconnect its pinned socket to a
// replacement process.
type tuiDaemonBackend struct {
	mu      sync.Mutex
	ensure  tuiClientFactory
	initial *client.Client
}

func (b *tuiDaemonBackend) acquire(ctx context.Context) (*client.Client, error) {
	b.mu.Lock()
	if b.initial != nil {
		c := b.initial
		b.initial = nil
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()
	return b.ensure(ctx)
}

func withTUIClient[T any](
	ctx context.Context, backend *tuiDaemonBackend,
	request func(*client.Client) (T, error),
) (T, error) {
	var zero T
	for attempt := range 2 {
		c, err := backend.acquire(ctx)
		if err != nil {
			if attempt == 0 &&
				errors.Is(err, client.ErrTransientDaemonAcquisition) &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return zero, err
		}
		result, requestErr := request(c)
		_ = c.Close()
		if requestErr == nil {
			return result, nil
		}
		if !client.IsTransportError(requestErr) ||
			errors.Is(requestErr, context.Canceled) ||
			errors.Is(requestErr, context.DeadlineExceeded) {
			return zero, requestErr
		}
		if attempt == 1 {
			return zero, fmt.Errorf("reconnecting to docbank daemon: %w", requestErr)
		}
	}
	return zero, errors.New("reconnecting to docbank daemon failed")
}

// withTUIMutationClient retries only daemon acquisition. Once a mutation is
// sent, a lost response is an unconfirmed outcome rather than permission to
// send the revision-bound operation again.
func withTUIMutationClient[T any](
	ctx context.Context, backend *tuiDaemonBackend,
	request func(*client.Client) (T, error),
) (T, error) {
	var zero T
	for attempt := range 2 {
		c, err := backend.acquire(ctx)
		if err != nil {
			if attempt == 0 &&
				errors.Is(err, client.ErrTransientDaemonAcquisition) &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return zero, err
		}
		result, requestErr := request(c)
		_ = c.Close()
		return result, requestErr
	}
	return zero, errors.New("acquiring docbank daemon failed")
}

func (b *tuiDaemonBackend) Stat(ctx context.Context, path string) (api.Node, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.Node, error) {
		return c.Stat(ctx, path)
	})
}

func (b *tuiDaemonBackend) Node(ctx context.Context, nodeID int64) (api.Node, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.Node, error) {
		return c.Node(ctx, nodeID)
	})
}

func (b *tuiDaemonBackend) ChildrenPage(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.NodePage, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.NodePage, error) {
		return c.ChildrenPage(ctx, nodeID, limit, offset)
	})
}

func (b *tuiDaemonBackend) Search(
	ctx context.Context, query string, limit int,
) (api.SearchReport, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.SearchReport, error) {
		return c.Search(ctx, query, limit)
	})
}

func (b *tuiDaemonBackend) NodeTags(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.TagPage, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.TagPage, error) {
		return c.NodeTags(ctx, nodeID, limit, offset)
	})
}

func (b *tuiDaemonBackend) Jobs(ctx context.Context) ([]api.Job, error) {
	return withTUIClient(ctx, b, func(c *client.Client) ([]api.Job, error) {
		return c.Jobs(ctx)
	})
}

func (b *tuiDaemonBackend) Info(ctx context.Context) (api.VaultInfo, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.VaultInfo, error) {
		return c.Info(ctx)
	})
}

func (b *tuiDaemonBackend) BackupList(
	ctx context.Context,
) ([]api.BackupSnapshot, error) {
	return withTUIClient(ctx, b, func(c *client.Client) ([]api.BackupSnapshot, error) {
		return c.BackupList(ctx, "")
	})
}

func (b *tuiDaemonBackend) TrashPage(
	ctx context.Context, limit, offset int,
) (api.TrashPage, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.TrashPage, error) {
		return c.TrashPage(ctx, limit, offset)
	})
}

func (b *tuiDaemonBackend) Trash(
	ctx context.Context, nodeID, revision int64,
) (api.Node, error) {
	node, err := withTUIMutationClient(ctx, b, func(c *client.Client) (api.Node, error) {
		return c.Trash(ctx, nodeID, revision)
	})
	if client.IsTransportError(err) || client.IsResponseDecodeError(err) {
		return api.Node{}, doctui.NewMutationUnconfirmedError("trash", err)
	}
	return node, err
}

func (b *tuiDaemonBackend) Restore(
	ctx context.Context, nodeID, revision int64,
) (api.Node, error) {
	node, err := withTUIMutationClient(ctx, b, func(c *client.Client) (api.Node, error) {
		return c.Restore(ctx, nodeID, revision)
	})
	if client.IsTransportError(err) || client.IsResponseDecodeError(err) {
		return api.Node{}, doctui.NewMutationUnconfirmedError("restore", err)
	}
	return node, err
}

func (b *tuiDaemonBackend) AuditHistory(
	ctx context.Context, path string, nodeID int64, limit int, cursor string,
) (api.AuditEventPage, error) {
	return withTUIClient(ctx, b, func(c *client.Client) (api.AuditEventPage, error) {
		return c.AuditHistory(ctx, path, nodeID, limit, cursor)
	})
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}
