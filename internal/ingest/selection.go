package ingest

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// sourceSelection is the request-scoped source matcher. Its patterns use
// path.Match syntax and are evaluated against source-root-relative slash paths.
type sourceSelection struct {
	include []sourceRule
	exclude []sourceRule
}

type sourceRule struct {
	pattern  string
	basename bool
}

func compileSourceSelection(opts Options) (sourceSelection, error) {
	include, err := compileSourceRules("include", opts.Include)
	if err != nil {
		return sourceSelection{}, err
	}
	exclude, err := compileSourceRules("exclude", opts.Exclude)
	if err != nil {
		return sourceSelection{}, err
	}
	return sourceSelection{include: include, exclude: exclude}, nil
}

func compileSourceRules(kind string, values []string) ([]sourceRule, error) {
	rules := make([]sourceRule, 0, len(values))
	for _, raw := range values {
		if raw == "" {
			return nil, fmt.Errorf("%s rule must not be empty", kind)
		}
		normalized := filepath.ToSlash(raw)
		if !filepath.IsLocal(filepath.FromSlash(normalized)) {
			return nil, fmt.Errorf("%s rule %q must be relative", kind, raw)
		}
		segments := strings.Split(normalized, "/")
		if slices.Contains(segments, "..") {
			return nil, fmt.Errorf("%s rule %q must not contain parent traversal", kind, raw)
		}
		pattern := path.Clean(normalized)
		if pattern == "." {
			return nil, fmt.Errorf("%s rule %q must name an entry within each source", kind, raw)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("%s rule %q is invalid: %w", kind, raw, err)
		}
		rules = append(rules, sourceRule{
			pattern:  pattern,
			basename: !strings.ContainsRune(normalized, '/'),
		})
	}
	return rules, nil
}

func (s sourceSelection) excluded(sourceRoot, sourcePath string) bool {
	return s.matches(sourceRoot, sourcePath, s.exclude)
}

func (s sourceSelection) included(sourceRoot, sourcePath string) bool {
	if len(s.include) == 0 {
		return true
	}
	return s.matches(sourceRoot, sourcePath, s.include)
}

func (s sourceSelection) matches(sourceRoot, sourcePath string, rules []sourceRule) bool {
	rel, err := filepath.Rel(sourceRoot, sourcePath)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	name := filepath.Base(sourcePath)
	for _, rule := range rules {
		subject := rel
		if rule.basename {
			subject = name
		}
		matched, err := path.Match(rule.pattern, subject)
		if err == nil && matched {
			return true
		}
	}
	return false
}
