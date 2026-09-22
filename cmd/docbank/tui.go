package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"uuid"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
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
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		backend := &tuiDaemonBackend{ensure: daemonconn.Ensure, initial: c}
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

type tuiClientFactory func(context.Context) (*daemonconn.Connection, error)

// tuiDaemonBackend reacquires the daemon around each bounded interaction. A
// TUI can remain open longer than the configured daemon idle timeout, and a
// proven client intentionally refuses to reconnect its pinned socket to a
// replacement process.
type tuiDaemonBackend struct {
	mu      sync.Mutex
	ensure  tuiClientFactory
	initial *daemonconn.Connection
}

func (b *tuiDaemonBackend) acquire(ctx context.Context) (*daemonconn.Connection, error) {
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
	request func(*daemonconn.Connection) (T, error),
) (T, error) {
	var zero T
	for attempt := range 2 {
		c, err := backend.acquire(ctx)
		if err != nil {
			if attempt == 0 &&
				errors.Is(err, daemonconn.ErrTransientDaemonAcquisition) &&
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
		if !daemonconn.IsTransportError(requestErr) ||
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
	request func(*daemonconn.Connection) (T, error),
) (T, error) {
	var zero T
	for attempt := range 2 {
		c, err := backend.acquire(ctx)
		if err != nil {
			if attempt == 0 &&
				errors.Is(err, daemonconn.ErrTransientDaemonAcquisition) &&
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
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.Node, error) {
		result, err := c.API().ResolvePath(ctx, &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: path}})
		if err != nil {
			var zero api.Node
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) Node(ctx context.Context, nodeID int64) (api.Node, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.Node, error) {
		result, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: nodeID}})
		if err != nil {
			var zero api.Node
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) ChildrenPage(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.NodePage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.NodePage, error) {
		return c.ChildrenPage(ctx, nodeID, limit, offset)
	})
}

func (b *tuiDaemonBackend) Search(
	ctx context.Context, query string, limit int,
) (api.SearchReport, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.SearchReport, error) {
		return c.Search(ctx, query, limit)
	})
}

func (b *tuiDaemonBackend) NodeTags(
	ctx context.Context, nodeID int64, limit, offset int,
) (api.TagPage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.TagPage, error) {
		return c.NodeTags(ctx, nodeID, limit, offset)
	})
}

func (b *tuiDaemonBackend) Jobs(ctx context.Context) ([]api.Job, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) ([]api.Job, error) {
		response, err := c.API().ListJobs(ctx)
		if err != nil {
			return nil, err
		}
		return response.Items, nil
	})
}

func (b *tuiDaemonBackend) Packages(ctx context.Context, direction, after string, limit int) (api.PackagePage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.PackagePage, error) {
		query := &apiclient.ListPackagesQuery{Limit: new(int64(limit))}
		if direction != "" {
			value := apiclient.ListPackagesQueryDirection(direction)
			query.Direction = &value
		}
		if after != "" {
			query.After = &after
		}
		result, err := c.API().ListPackages(ctx, &apiclient.ListPackagesRequestOptions{Query: query})
		if err != nil {
			return api.PackagePage{}, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) PackageMembers(ctx context.Context, packageID string, afterOrdinal, limit int) (api.PackageMemberPage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.PackageMemberPage, error) {
		result, err := c.API().ListPackageMembers(ctx, &apiclient.ListPackageMembersRequestOptions{
			PathParams: &apiclient.ListPackageMembersPath{PackageID: packageID},
			Query:      &apiclient.ListPackageMembersQuery{AfterOrdinal: new(int64(afterOrdinal)), Limit: new(int64(limit))},
		})
		if err != nil {
			return api.PackageMemberPage{}, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) LookupLabel(ctx context.Context, label, packageID string, limit int) (api.PackageLabelCandidatePage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.PackageLabelCandidatePage, error) {
		query := &apiclient.ListPackageLabelCandidatesQuery{Label: label, Limit: new(int64(limit))}
		if packageID != "" {
			query.PackageID = &packageID
		}
		result, err := c.API().ListPackageLabelCandidates(ctx, &apiclient.ListPackageLabelCandidatesRequestOptions{Query: query})
		if err != nil {
			return api.PackageLabelCandidatePage{}, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) Info(ctx context.Context) (api.VaultInfo, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.VaultInfo, error) {
		result, err := c.API().VaultInfo(ctx)
		if err != nil {
			var zero api.VaultInfo
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) BackupList(
	ctx context.Context,
) ([]api.BackupSnapshot, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) ([]api.BackupSnapshot, error) {
		response, err := c.API().ListBackupSnapshots(ctx, &apiclient.ListBackupSnapshotsRequestOptions{Query: &apiclient.ListBackupSnapshotsQuery{Repo: new("")}})
		if err != nil {
			return nil, err
		}
		return response.Items, nil
	})
}

func (b *tuiDaemonBackend) ProcessingProfiles(
	ctx context.Context,
) ([]api.ProcessingProfileSummary, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) ([]api.ProcessingProfileSummary, error) {
		result, err := c.API().ListDocumentProcessingProfiles(ctx)
		if err != nil {
			var zero []api.ProcessingProfileSummary
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) ResolveDocumentSourceFence(
	ctx context.Context, request api.DocumentSourceFenceResolveRequest,
) (api.DocumentSourceFenceResolution, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.DocumentSourceFenceResolution, error) {
		return c.ResolveDocumentSourceFence(ctx, request)
	})
}

func (b *tuiDaemonBackend) PlanProcessing(
	ctx context.Context, request api.ProcessingPlanRequest,
) (api.ProcessingPlan, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.ProcessingPlan, error) {
		result, err := c.API().PlanDocumentProcessing(ctx, &apiclient.PlanDocumentProcessingRequestOptions{Body: new(request)})
		if err != nil {
			var zero api.ProcessingPlan
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) DocumentCoverage(
	ctx context.Context, profile string, fence api.DocumentSourceFence,
) (api.CoverageReport, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.CoverageReport, error) {
		vaultID, err := uuid.Parse(fence.VaultUID)
		if err != nil {
			return api.CoverageReport{}, fmt.Errorf("parsing coverage vault ID: %w", err)
		}
		report, err := c.API().GetDocumentProcessingCoverage(ctx, &apiclient.GetDocumentProcessingCoverageRequestOptions{Query: &apiclient.GetDocumentProcessingCoverageQuery{Profile: profile, VaultUID: vaultID, ContentVersionID: fence.ContentVersionIDs}})
		if err != nil {
			return api.CoverageReport{}, err
		}
		return *report, nil
	})
}

func (b *tuiDaemonBackend) SearchDocuments(
	ctx context.Context, request api.DocumentSearchRequest,
) (api.DocumentSearchReport, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.DocumentSearchReport, error) {
		return c.SearchDocuments(ctx, request)
	})
}

func (b *tuiDaemonBackend) SimilarDocuments(ctx context.Context, request api.DocumentSimilarRequest) (api.DocumentSimilarReport, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.DocumentSimilarReport, error) {
		return c.SimilarDocuments(ctx, request)
	})
}

func (b *tuiDaemonBackend) StartProcessingStream(
	ctx context.Context, request api.StartProcessingRequest, profileFingerprint string,
) (doctui.ProcessingEventStream, error) {
	return withTUIMutationClient(ctx, b, func(c *daemonconn.Connection) (doctui.ProcessingEventStream, error) {
		return c.StartProcessingStream(ctx, request, profileFingerprint)
	})
}

func (b *tuiDaemonBackend) ProcessingStatus(
	ctx context.Context, jobID string,
) (api.ProcessingStatus, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.ProcessingStatus, error) {
		return c.ProcessingStatus(ctx, jobID)
	})
}

func (b *tuiDaemonBackend) RenditionForSelector(
	ctx context.Context, selector api.ProcessingSelector, maxBytes int64,
) (doctui.Rendition, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (doctui.Rendition, error) {
		stream, err := c.RenditionForSelector(ctx, selector, maxBytes)
		if err != nil {
			return doctui.Rendition{}, err
		}
		var markdown bytes.Buffer
		_, copyErr := stream.CopyVerified(&markdown)
		if err := errors.Join(copyErr, stream.Close()); err != nil {
			return doctui.Rendition{}, err
		}
		return doctui.Rendition{
			Markdown: markdown.String(), AttachmentID: stream.AttachmentID,
			BuildID: stream.BuildID, ArtifactID: stream.ArtifactID,
			SHA256: stream.BlobHash, Size: stream.Size,
			Completeness: stream.Completeness, Warnings: stream.Warnings,
		}, nil
	})
}

func (b *tuiDaemonBackend) TrashPage(
	ctx context.Context, limit, offset int,
) (api.TrashPage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.TrashPage, error) {
		result, err := c.API().ListTrash(ctx, &apiclient.ListTrashRequestOptions{Query: &apiclient.ListTrashQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
		if err != nil {
			var zero api.TrashPage
			return zero, err
		}
		return *result, nil
	})
}

func (b *tuiDaemonBackend) Trash(
	ctx context.Context, nodeID, revision int64,
) (api.Node, error) {
	node, err := withTUIMutationClient(ctx, b, func(c *daemonconn.Connection) (api.Node, error) {
		result, err := c.API().TrashNode(ctx, &apiclient.TrashNodeRequestOptions{PathParams: &apiclient.TrashNodePath{ID: nodeID}, Header: &apiclient.TrashNodeHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
		if err != nil {
			var zero api.Node
			return zero, err
		}
		return *result, nil
	})
	if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) {
		return api.Node{}, doctui.NewMutationUnconfirmedError("trash", err)
	}
	return node, err
}

func (b *tuiDaemonBackend) Restore(
	ctx context.Context, nodeID, revision int64,
) (api.Node, error) {
	node, err := withTUIMutationClient(ctx, b, func(c *daemonconn.Connection) (api.Node, error) {
		result, err := c.API().RestoreNode(ctx, &apiclient.RestoreNodeRequestOptions{PathParams: &apiclient.RestoreNodePath{ID: nodeID}, Header: &apiclient.RestoreNodeHeaders{IfMatch: strconv.Quote(strconv.FormatInt(revision, 10))}})
		if err != nil {
			var zero api.Node
			return zero, err
		}
		return *result, nil
	})
	if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) {
		return api.Node{}, doctui.NewMutationUnconfirmedError("restore", err)
	}
	return node, err
}

func (b *tuiDaemonBackend) AuditHistory(
	ctx context.Context, path string, nodeID int64, limit int, cursor string,
) (api.AuditEventPage, error) {
	return withTUIClient(ctx, b, func(c *daemonconn.Connection) (api.AuditEventPage, error) {
		return c.AuditHistory(ctx, path, nodeID, limit, cursor)
	})
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}
