package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kit/pack"
)

const (
	maxAuxiliaryArtifacts    = 64
	maxAuxiliaryBytes        = int64(64 << 20)
	auxiliaryRollbackTimeout = 10 * time.Second
)

var auxiliaryNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// AuxiliaryArtifact is one application-defined, immutable snapshot artifact.
// Open must return the same bytes and exact size for the lifetime of its
// originating snapshot.
type AuxiliaryArtifact struct {
	Name   string
	Format string
	Open   func(context.Context) (io.ReadCloser, int64, error)
}

// AuxiliarySource optionally extends a pinned MetadataSnapshot or FrozenView
// with application-neutral auxiliary artifacts captured under the same
// preservation boundary.
type AuxiliarySource interface {
	AuxiliaryArtifacts(context.Context) ([]AuxiliaryArtifact, error)
}

// RestoredAuxiliary carries independently verified artifact bytes to the
// application before target publication.
type RestoredAuxiliary struct {
	Name   string
	Format string
	SHA256 string
	Data   []byte
}

// AuxiliaryTarget stages verified application-defined artifacts without
// making them externally visible. A StageAuxiliary error must leave no work
// for Kit to clean up. Kit never interprets artifact payloads.
type AuxiliaryTarget interface {
	StageAuxiliary(context.Context, []RestoredAuxiliary) (AuxiliaryRestore, error)
}

// AuxiliaryRestore is one staged auxiliary-state replacement. Commit runs
// only after Kit has published and durably synced the restored target.
// Rollback must discard all staged work and clean up any partial effects from
// a failed Commit. Kit calls Commit at most once and, if Commit does not
// succeed, Rollback exactly once with an independently bounded context.
type AuxiliaryRestore interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

// ManifestAuxiliary identifies one content-addressed auxiliary artifact.
type ManifestAuxiliary struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Blob   string `json:"blob"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func auxiliaryArtifacts(
	ctx context.Context,
	source any,
) ([]AuxiliaryArtifact, error) {
	auxiliary, ok := source.(AuxiliarySource)
	if !ok {
		return nil, nil
	}
	artifacts, err := auxiliary.AuxiliaryArtifacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading auxiliary artifact list: %w", err)
	}
	if err := validateAuxiliaryArtifacts(artifacts); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func validateAuxiliaryArtifacts(artifacts []AuxiliaryArtifact) error {
	if len(artifacts) > maxAuxiliaryArtifacts {
		return fmt.Errorf(
			"backup: auxiliary artifact count %d exceeds %d",
			len(artifacts), maxAuxiliaryArtifacts,
		)
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if !auxiliaryNamePattern.MatchString(artifact.Name) {
			return fmt.Errorf("backup: invalid auxiliary artifact name %q", artifact.Name)
		}
		if err := validateAuxiliaryFormat(artifact.Format); err != nil {
			return err
		}
		if artifact.Open == nil {
			return fmt.Errorf("backup: auxiliary artifact %q has no opener", artifact.Name)
		}
		if _, duplicate := seen[artifact.Name]; duplicate {
			return fmt.Errorf("backup: duplicate auxiliary artifact name %q", artifact.Name)
		}
		seen[artifact.Name] = struct{}{}
	}
	return nil
}

func validateAuxiliaryFormat(format string) error {
	if format == "" || len(format) > 128 || !utf8.ValidString(format) ||
		strings.IndexFunc(format, unicode.IsControl) >= 0 {
		return fmt.Errorf("backup: invalid auxiliary artifact format %q", format)
	}
	return nil
}

func captureAuxiliaryArtifacts(
	ctx context.Context,
	repo *Repo,
	artifacts []AuxiliaryArtifact,
	zstdLevel int,
	appender *PackAppender,
) ([]ManifestAuxiliary, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	sorted := append([]AuxiliaryArtifact(nil), artifacts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]ManifestAuxiliary, 0, len(sorted))
	var total int64
	for _, artifact := range sorted {
		reader, size, err := artifact.Open(ctx)
		if err != nil {
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			return nil, fmt.Errorf("backup: opening auxiliary artifact %q: %w", artifact.Name, err)
		}
		if reader == nil || size < 0 || size > maxAuxiliaryBytes-total ||
			uint64(size) > pack.MaxRawLen {
			if reader != nil {
				_ = reader.Close()
			}
			return nil, fmt.Errorf(
				"backup: auxiliary artifact %q has invalid size %d",
				artifact.Name, size,
			)
		}
		prepared, prepareErr := pack.PrepareBlob(
			ctx, reader, uint64(size), zstdLevel,
			pack.AppendStreamOptions{ScratchDir: repo.Path(stagingDirName)},
		)
		closeErr := reader.Close()
		if err := errors.Join(prepareErr, closeErr); err != nil {
			if prepared != nil {
				_ = prepared.Close()
			}
			return nil, fmt.Errorf("backup: preparing auxiliary artifact %q: %w", artifact.Name, err)
		}
		if prepared == nil {
			return nil, fmt.Errorf("backup: preparing auxiliary artifact %q returned no result", artifact.Name)
		}
		id := prepared.ID()
		if _, err := appender.AddPrepared(ctx, prepared); err != nil {
			return nil, err
		}
		total += size
		out = append(out, ManifestAuxiliary{
			Name: artifact.Name, Format: artifact.Format,
			Blob: id.String(), Bytes: size, SHA256: id.String(),
		})
	}
	return out, nil
}

func validateManifestAuxiliary(artifacts []ManifestAuxiliary) error {
	if len(artifacts) > maxAuxiliaryArtifacts {
		return fmt.Errorf(
			"backup: auxiliary artifact count %d exceeds %d",
			len(artifacts), maxAuxiliaryArtifacts,
		)
	}
	var total int64
	previous := ""
	for _, artifact := range artifacts {
		if !auxiliaryNamePattern.MatchString(artifact.Name) {
			return fmt.Errorf("backup: invalid auxiliary artifact name %q", artifact.Name)
		}
		if artifact.Name <= previous {
			return errors.New("backup: auxiliary artifacts are not uniquely sorted by name")
		}
		previous = artifact.Name
		if err := validateAuxiliaryFormat(artifact.Format); err != nil {
			return err
		}
		if artifact.Bytes < 0 || artifact.Bytes > maxAuxiliaryBytes-total {
			return fmt.Errorf(
				"backup: auxiliary artifact %q has invalid size %d",
				artifact.Name, artifact.Bytes,
			)
		}
		total += artifact.Bytes
		id, err := pack.ParseBlobID(artifact.Blob)
		if err != nil {
			return fmt.Errorf(
				"backup: auxiliary artifact %q blob id %q: %w",
				artifact.Name, artifact.Blob, err,
			)
		}
		if artifact.SHA256 != id.String() {
			return fmt.Errorf(
				"backup: auxiliary artifact %q digest differs from blob identity",
				artifact.Name,
			)
		}
	}
	return nil
}
