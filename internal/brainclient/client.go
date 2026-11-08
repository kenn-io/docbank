package brainclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/push"
	"go.kenn.io/docbank/internal/store"
)

// Dependencies keep connection acquisition and publishing on the existing
// daemon transport. Tests may supply synthetic, ownership-proven endpoints.
type Dependencies struct {
	Ensure           func(context.Context, string) (*daemonconn.Connection, error)
	Running          func(context.Context, string) (*daemonconn.Connection, bool, error)
	Remote           func(string, string) (*daemonconn.Connection, error)
	ReadKey          func(string) (string, error)
	Publish          func(context.Context, *daemonconn.Connection, push.Options) (push.Report, error)
	ServiceInstalled func(context.Context, string) (bool, error)
	Now              func() time.Time
}

type Client struct {
	registry *Registry
	deps     Dependencies
}

func New(registry *Registry, deps Dependencies) *Client {
	if deps.Ensure == nil {
		deps.Ensure = daemonconn.EnsureFor
	}
	if deps.Running == nil {
		deps.Running = daemonconn.RunningFor
	}
	if deps.Remote == nil {
		deps.Remote = daemonconn.NewPushConnection
	}
	if deps.ReadKey == nil {
		deps.ReadKey = ReadKey
	}
	if deps.Publish == nil {
		deps.Publish = push.Run
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Client{registry: registry, deps: deps}
}

func (c *Client) connection(ctx context.Context, cfg ConnectionConfig, start bool) (*daemonconn.Connection, bool, error) {
	if cfg.LocalVaultRoot != "" {
		if start {
			conn, err := c.deps.Ensure(ctx, cfg.LocalVaultRoot)
			return conn, err == nil, err
		}
		return c.deps.Running(ctx, cfg.LocalVaultRoot)
	}
	key, err := c.deps.ReadKey(cfg.KeyFile)
	if err != nil {
		return nil, false, err
	}
	conn, err := c.deps.Remote(cfg.RemoteOrigin, key)
	return conn, err == nil, err
}

// Connect saves only a validated source and target. IndexLocal explicitly
// grants the daemon's ordinary consent only after validating its fixed plan.
func (c *Client) Connect(ctx context.Context, cfg ConnectionConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	info, err := os.Stat(cfg.SourceRoot)
	if err != nil {
		return fmt.Errorf("opening source directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("source must be a directory")
	}
	saved, err := c.registry.Get(ctx, cfg.Name)
	if err == nil {
		if equalConfig(saved, cfg) {
			return nil
		}
		return fmt.Errorf("connection already exists with different settings: %w", fs.ErrExist)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	conn, _, err := c.connection(ctx, cfg, true)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ListDocuments(ctx, api.DocumentQuery{PublishedNotes: &api.PublishedNoteSelector{Publication: cfg.Name}, PageSize: 1}); err != nil {
		return fmt.Errorf("checking daemon capability: %w", err)
	}
	granted := false
	if cfg.IndexLocal {
		plan, err := conn.NoteLocalTextPlan(ctx)
		if err != nil {
			return fmt.Errorf("reading local text plan: %w", err)
		}
		if err := api.ValidateNoteLocalTextPlan(plan); err != nil {
			return err
		}
		if plan.Plan.ConsentRequired {
			if _, err := conn.GrantScopedProcessingConsent(ctx, plan.ConsentRequest); err != nil {
				return fmt.Errorf("local text consent request failed; check daemon consent before retrying because it may have succeeded: %w", err)
			}
			granted = true
		}
	}
	if err := c.registry.Put(ctx, cfg); err != nil {
		if granted {
			return fmt.Errorf("saving connection failed after local text consent was granted; consent may remain active: %w", err)
		}
		return err
	}
	return nil
}

// Run holds the saved name's lock for the entire managed watch lifetime.
func (c *Client) Run(ctx context.Context, name string, status func(context.Context, push.PublicationStatus) error) (report push.Report, retErr error) {
	release, err := c.registry.AcquireRun(ctx, name)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	cfg, err := c.registry.Get(ctx, name)
	if err != nil {
		return report, err
	}
	conn, _, err := c.connection(ctx, cfg, true)
	if err != nil {
		return report, err
	}
	var mu sync.Mutex
	connections := []*daemonconn.Connection{conn}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, item := range connections {
			retErr = errors.Join(retErr, item.Close())
		}
	}()
	opts := push.Options{Publication: true, Watch: true, Status: status, Folder: config.WatchConfig{
		Name: cfg.Name, Source: cfg.SourceRoot, Destination: cfg.Destination,
		SettleTime: config.Duration(cfg.SettleTime), MinimumAge: config.Duration(cfg.MinimumAge),
		ScanInterval: config.Duration(cfg.ScanInterval), Exclude: append([]string(nil), cfg.Exclude...),
	}}
	if cfg.LocalVaultRoot != "" {
		opts.Reconnect = func(ctx context.Context) (*daemonconn.Connection, error) {
			next, err := c.deps.Ensure(ctx, cfg.LocalVaultRoot)
			if err == nil {
				mu.Lock()
				connections = append(connections, next)
				mu.Unlock()
			}
			return next, err
		}
	}
	return c.deps.Publish(ctx, conn, opts)
}

// Disconnect removes only client configuration and requires an explicit
// launcher ownership check; archived data and publication membership remain.
func (c *Client) Disconnect(ctx context.Context, name string) error {
	if c.deps.ServiceInstalled == nil {
		return errors.New("launcher status is unavailable")
	}
	installed, err := c.deps.ServiceInstalled(ctx, name)
	if err != nil {
		return err
	}
	if installed {
		return errors.New("uninstall the publisher service before disconnecting")
	}
	return c.registry.Remove(ctx, name)
}

// AcceptLocal authorizes one later local upload against the observed archive
// head and source revision; it does not modify archived content.
func (c *Client) AcceptLocal(ctx context.Context, name, sourceRef, expectedHead string) (api.NotePublicationRecoveryGrant, error) {
	if store.ValidatePushName(name) != nil || store.ValidatePushRef(sourceRef) != nil || store.ValidatePushRunID(expectedHead) != nil {
		return api.NotePublicationRecoveryGrant{}, errors.New("invalid note recovery request")
	}
	cfg, err := c.registry.Get(ctx, name)
	if err != nil {
		return api.NotePublicationRecoveryGrant{}, err
	}
	conn, _, err := c.connection(ctx, cfg, true)
	if err != nil {
		return api.NotePublicationRecoveryGrant{}, err
	}
	defer func() { _ = conn.Close() }()
	snapshot, err := conn.NotePublicationSource(ctx, name, sourceRef)
	if err != nil {
		return api.NotePublicationRecoveryGrant{}, err
	}
	if snapshot.Source.ArchiveHead == nil || snapshot.Source.ArchiveHead.ContentVersionID != expectedHead {
		return api.NotePublicationRecoveryGrant{}, store.ErrPublicationSourceStale
	}
	return conn.AcceptLocalPublishedNote(ctx, api.NotePublicationAcceptLocalRequest{
		Name: name, DatabaseEpoch: snapshot.DatabaseEpoch, SourceRef: sourceRef,
		ExpectedHeadVersionID: expectedHead, ExpectedSourceRevision: snapshot.Source.Identity.SourceRevision,
		OperationID: uuid.NewV4().String(),
	})
}
