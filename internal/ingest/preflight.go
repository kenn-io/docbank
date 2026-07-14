package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/docbank/internal/blob"
)

const (
	maxPreflightFindings  = 100
	maxPreflightFileTypes = 50
)

// Options controls source selection for both preflight and the real import.
// A rule without a slash matches that entry name anywhere. A relative path
// rule matches that entry and its descendants within each supplied source.
type Options struct {
	Exclude []string
	// Progress receives bounded updates while a real ingest reads and commits
	// files. Preflight ignores it because it never opens file content.
	Progress func(ProgressEvent)
}

// SizeClass summarizes one physical-policy outcome.
type SizeClass struct {
	Files int64
	Bytes int64
}

// FileType summarizes regular files by lowercase extension. Extension is
// empty for names without an extension.
type FileType struct {
	Extension string
	Files     int64
	Bytes     int64
}

// PreflightFinding is a bounded sample of an exclusion, skipped non-regular
// entry, or filesystem error. Counts in PreflightReport remain authoritative
// when more than maxPreflightFindings findings exist.
type PreflightFinding struct {
	Path   string
	Kind   string
	Detail string
}

// PreflightReport is a metadata-only inventory. It never opens regular-file
// content and never touches the vault store.
type PreflightReport struct {
	Files        int64
	Directories  int64
	LogicalBytes int64

	PackEligible SizeClass
	LooseOnly    SizeClass
	Rejected     SizeClass

	Excluded int64
	Skipped  int64
	Errors   int64

	FileTypes          []FileType
	OtherFileTypes     SizeClass
	FileTypesTruncated bool
	Findings           []PreflightFinding
	FindingsTruncated  bool
}

type exclusionRule struct {
	name string
	path string
}

type exclusions struct {
	rules []exclusionRule
}

func compileExclusions(values []string) (exclusions, error) {
	result := exclusions{rules: make([]exclusionRule, 0, len(values))}
	for _, raw := range values {
		if raw == "" {
			return exclusions{}, errors.New("exclude rule must not be empty")
		}
		converted := filepath.FromSlash(raw)
		if !filepath.IsLocal(converted) {
			return exclusions{}, fmt.Errorf("exclude rule %q must be relative", raw)
		}
		if slices.Contains(strings.Split(converted, string(filepath.Separator)), "..") {
			return exclusions{}, fmt.Errorf("exclude rule %q must not contain parent traversal", raw)
		}
		pathForm := strings.ContainsRune(converted, filepath.Separator)
		clean := filepath.Clean(converted)
		if clean == "." {
			return exclusions{}, fmt.Errorf("exclude rule %q must name an entry within each source", raw)
		}
		rule := exclusionRule{}
		if pathForm {
			rule.path = clean
		} else {
			rule.name = clean
		}
		result.rules = append(result.rules, rule)
	}
	return result, nil
}

func (e exclusions) match(sourceRoot, sourcePath string) bool {
	rel, err := filepath.Rel(sourceRoot, sourcePath)
	if err != nil {
		return false
	}
	name := filepath.Base(sourcePath)
	for _, rule := range e.rules {
		if rule.name != "" && name == rule.name {
			return true
		}
		if rule.path != "" && (rel == rule.path || strings.HasPrefix(rel, rule.path+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// ValidateOptions checks source-selection rules without touching the
// filesystem. API handlers use it to return a validation error before work.
func ValidateOptions(opts Options) error {
	_, err := compileExclusions(opts.Exclude)
	return err
}

// Preflight inventories sources using the same root-symlink and exclusion
// semantics as AddPathsWithOptions. It uses directory metadata only: cloud
// placeholders are not opened or hydrated merely to estimate an import.
func Preflight(ctx context.Context, sources []string, opts Options) (PreflightReport, error) {
	var report PreflightReport
	excludes, err := compileExclusions(opts.Exclude)
	if err != nil {
		return report, err
	}
	types := make(map[string]FileType)
	for _, rawSource := range sources {
		source := filepath.Clean(rawSource)
		if err := ctx.Err(); err != nil {
			return report, err
		}
		info, err := os.Lstat(source)
		if err != nil {
			report.addFinding(source, "error", err.Error())
			continue
		}
		if excludes.match(source, source) {
			report.addFinding(source, "excluded", "matched an exclusion rule")
			continue
		}
		walkRoot := source
		if info.Mode()&fs.ModeSymlink != 0 {
			walkRoot, err = filepath.EvalSymlinks(source)
			if err != nil {
				report.addFinding(source, "error", "resolving explicitly named directory symlink: "+err.Error())
				continue
			}
			info, err = os.Stat(walkRoot)
			if err != nil {
				report.addFinding(source, "error", "checking explicitly named directory symlink: "+err.Error())
				continue
			}
			if !info.IsDir() {
				report.addFinding(source, "skipped", "explicit symlink source does not resolve to a directory")
				continue
			}
		}
		if info.Mode().IsRegular() {
			report.addFile(source, info.Size(), types)
			continue
		}
		if !info.IsDir() {
			report.addFinding(source, "skipped", "not a regular file or directory")
			continue
		}
		if err := preflightTree(ctx, &report, types, excludes, source, walkRoot); err != nil {
			return report, err
		}
	}
	report.FileTypes, report.OtherFileTypes, report.FileTypesTruncated = sortedFileTypes(types)
	return report, nil
}

func preflightTree(
	ctx context.Context,
	report *PreflightReport,
	types map[string]FileType,
	excludes exclusions,
	sourceRoot, walkRoot string,
) error {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolving source %s: %w", sourceRoot, err)
	}
	walkRoot, err = filepath.Abs(walkRoot)
	if err != nil {
		return fmt.Errorf("resolving traversal root %s: %w", walkRoot, err)
	}
	if filepath.Base(sourceRoot) == string(filepath.Separator) || filepath.Base(walkRoot) == string(filepath.Separator) {
		return fmt.Errorf("cannot scan filesystem root %q", sourceRoot)
	}
	return filepath.WalkDir(walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sourcePath := sourceTreePath(sourceRoot, walkRoot, path)
		if walkErr != nil {
			report.addFinding(sourcePath, "error", walkErr.Error())
			return nil
		}
		if excludes.match(sourceRoot, sourcePath) {
			report.addFinding(sourcePath, "excluded", "matched an exclusion rule")
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case entry.IsDir():
			report.Directories++
		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				report.addFinding(sourcePath, "error", err.Error())
				return nil
			}
			report.addFile(sourcePath, info.Size(), types)
		default:
			report.addFinding(sourcePath, "skipped", "not a regular file (symlinks are skipped)")
		}
		return nil
	})
}

func (r *PreflightReport) addFile(path string, size int64, types map[string]FileType) {
	r.Files++
	r.LogicalBytes += size
	switch {
	case size <= blob.MaxPackedBlobBytes:
		r.PackEligible.Files++
		r.PackEligible.Bytes += size
	case size <= blob.MaxIngestBytes:
		r.LooseOnly.Files++
		r.LooseOnly.Bytes += size
	default:
		r.Rejected.Files++
		r.Rejected.Bytes += size
	}
	ext := strings.ToLower(filepath.Ext(path))
	item := types[ext]
	item.Extension = ext
	item.Files++
	item.Bytes += size
	types[ext] = item
}

func (r *PreflightReport) addFinding(path, kind, detail string) {
	switch kind {
	case "excluded":
		r.Excluded++
	case "skipped":
		r.Skipped++
	case "error":
		r.Errors++
	}
	if len(r.Findings) < maxPreflightFindings {
		r.Findings = append(r.Findings, PreflightFinding{Path: path, Kind: kind, Detail: detail})
	} else {
		r.FindingsTruncated = true
	}
}

func sortedFileTypes(types map[string]FileType) ([]FileType, SizeClass, bool) {
	result := make([]FileType, 0, len(types))
	for _, item := range types {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Bytes != result[j].Bytes {
			return result[i].Bytes > result[j].Bytes
		}
		if result[i].Files != result[j].Files {
			return result[i].Files > result[j].Files
		}
		return result[i].Extension < result[j].Extension
	})
	if len(result) <= maxPreflightFileTypes {
		return result, SizeClass{}, false
	}
	var other SizeClass
	for _, item := range result[maxPreflightFileTypes:] {
		other.Files += item.Files
		other.Bytes += item.Bytes
	}
	return result[:maxPreflightFileTypes], other, true
}
