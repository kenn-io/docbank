package atomicfile

import (
	"errors"
	"io/fs"
)

// defaultPerm is the permission of a newly created file when no option
// chooses another.
const defaultPerm fs.FileMode = 0o600

// Option configures WriteFile, Create, and WriteNew.
type Option func(*config)

type config struct {
	perm         fs.FileMode
	permSet      bool
	createPerm   bool
	preserveMode bool
	private      bool
	followLink   bool
	noSync       bool
	stagingDir   string
}

// WithPerm sets the permission bits of the written file. The bits are
// applied exactly, without the process umask. The default is 0600.
func WithPerm(perm fs.FileMode) Option {
	return func(c *config) {
		c.perm = perm
		c.permSet = true
	}
}

// WithCreatePerm sets the permission of a newly created file the way the
// perm argument of os.WriteFile does: the process umask filters it. Add
// WithPreserveMode to keep an existing target's mode, as os.WriteFile does.
// It cannot be combined with WithPerm or WithPrivate.
func WithCreatePerm(perm fs.FileMode) Option {
	return func(c *config) {
		c.perm = perm
		c.createPerm = true
	}
}

// WithPreserveMode keeps the permission bits of the existing target when it
// is a regular file. Otherwise the WithPerm value, the WithCreatePerm value
// filtered by the umask, or the default applies.
func WithPreserveMode() Option {
	return func(c *config) { c.preserveMode = true }
}

// WithPrivate stages the file with safefileio.CreatePrivateTemp, so the
// result is private to the current user: mode 0600 on Unix, a protected
// current-user DACL on Windows. It cannot be combined with WithPerm,
// WithCreatePerm or WithPreserveMode.
func WithPrivate() Option {
	return func(c *config) { c.private = true }
}

// WithFollowLink writes through a symlink or junction at the target's final
// component: the link chain is resolved (at most 40 links, relative
// destinations the way the system resolves them) and the file it names is
// replaced, leaving the links in place. Commit resolves the chain again and
// fails without writing if it no longer leads to the file chosen at Create.
// Without it a link at the target is refused with an error wrapping
// fslink.ErrIsLink. WriteNew rejects it.
func WithFollowLink() Option {
	return func(c *config) { c.followLink = true }
}

// WithoutSync skips the file and directory fsyncs. The write is still atomic
// for readers but may not survive a crash; use it only for disposable data
// such as caches.
func WithoutSync() Option {
	return func(c *config) { c.noSync = true }
}

// WithStagingDir stages the temporary file in dir instead of the target's
// directory. dir must be on the same volume as the target: a cross-volume
// rename fails and its error is returned; the content is never copied. When
// dir differs from the target's directory, both are fsynced after
// publication. Like the target's directory, dir must not be modifiable by
// untrusted users: the staging file is handled by pathname.
func WithStagingDir(dir string) Option {
	return func(c *config) { c.stagingDir = dir }
}

func newConfig(opts []Option) (config, error) {
	cfg := config{perm: defaultPerm}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.private && (cfg.permSet || cfg.createPerm || cfg.preserveMode) {
		return config{}, errors.New("WithPrivate cannot be combined with WithPerm, WithCreatePerm or WithPreserveMode")
	}
	if cfg.permSet && cfg.createPerm {
		return config{}, errors.New("WithPerm cannot be combined with WithCreatePerm")
	}
	return cfg, nil
}
