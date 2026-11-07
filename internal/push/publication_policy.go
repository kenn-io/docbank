package push

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"path"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/config"
)

func isPublishedMarkdown(ref string) bool {
	// Extensions are ASCII; Unicode case mappings do not expand the scope.
	ext := path.Ext(ref)
	var b strings.Builder
	for _, c := range ext {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String() == ".md" || b.String() == ".markdown"
}
func publicationPolicy(cfg config.WatchConfig) (config.WatchConfig, string, error) {
	if len(cfg.ExcludePatterns) > 0 {
		return cfg, "", errors.New("managed publications accept literal basename exclusions only")
	}
	cfg.Exclude = append([]string{".git", ".hg", ".svn", ".obsidian", ".trash"}, cfg.Exclude...)
	slices.Sort(cfg.Exclude)
	cfg.Exclude = slices.Compact(cfg.Exclude)
	b, err := json.Marshal(struct {
		Version    string   `json:"version"`
		Extensions []string `json:"extensions"`
		Excludes   []string `json:"excludes"`
	}{"markdown-brain-v1", []string{".md", ".markdown"}, cfg.Exclude})
	if err != nil {
		return cfg, "", err
	}
	hash := sha256.Sum256(b)
	return cfg, hex.EncodeToString(hash[:]), nil
}
