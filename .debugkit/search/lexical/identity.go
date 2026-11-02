package lexical

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

const (
	// KindLiteral quotes search terms and leaves tokenization to the index.
	KindLiteral = "literal"
	// KindCharacterPhrase splits Han, Hiragana, Katakana, and Hangul into
	// adjacent character tokens. It does not apply a morphological dictionary.
	KindCharacterPhrase = "character-phrase"
	// KindChinese is caller-supplied Chinese segmentation, typically cppjieba.
	// The runtime fingerprint distinguishes dictionary and library revisions.
	KindChinese = "chinese-cppjieba"
)

const (
	literalVersion         = "v1"
	characterPhraseVersion = "v1"
	// ChineseQueryVersion is the query-format revision mixed into a Chinese
	// runtime fingerprint by callers. Dictionary bytes are hashed separately.
	ChineseQueryVersion = "chinese-cppjieba-v1"
)

// Identity is the analyzer contract shared by indexing and query preparation.
// Equal identities are required before a prepared query is safe to run.
// Operational limits such as candidate windows are not part of it.
type Identity struct {
	Kind    string
	Version string
	// Runtime is empty for analyzers that have no external dictionary.
	// For Chinese segmentation it is the runtime fingerprint.
	Runtime string
}

// ErrIdentityMismatch reports that a query was prepared by a different
// analyzer than the index, so its tokens are not the index tokens.
var ErrIdentityMismatch = errors.New("lexical: analyzer identity mismatch")

// CheckCompatible returns an error wrapping ErrIdentityMismatch unless an
// index identity and a query identity can be used together. Identity values
// are comparable, so == is the plain yes/no test.
func CheckCompatible(index, query Identity) error {
	if index == query {
		return nil
	}
	return fmt.Errorf("%w: index %s, query %s", ErrIdentityMismatch, index, query)
}

func (id Identity) String() string {
	parts := []string{id.Kind, id.Version}
	if id.Runtime != "" {
		parts = append(parts, id.Runtime)
	}
	return strings.Join(parts, "/")
}

func literalIdentity() Identity {
	return Identity{Kind: KindLiteral, Version: literalVersion}
}

func characterPhraseIdentity() Identity {
	return Identity{Kind: KindCharacterPhrase, Version: characterPhraseVersion}
}

// RuntimeFile is one named blob that participates in a Chinese runtime
// fingerprint. Name is the file's base name. Data is its full contents.
type RuntimeFile struct {
	Name string
	Data []byte
}

// FingerprintRuntime hashes a schema version and ordered runtime files.
// Changing the version, a file name, file order, or any byte changes the
// result. The caller supplies library and dictionary bytes; nothing is read
// from disk. Typical cppjieba inputs are the extension library plus
// hmm_model.utf8, idf.utf8, jieba.dict.utf8, stop_words.utf8, and
// user.dict.utf8.
func FingerprintRuntime(version string, files []RuntimeFile) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", errors.New("lexical: runtime fingerprint version is required")
	}
	if len(files) == 0 {
		return "", errors.New("lexical: runtime fingerprint requires files")
	}
	h := sha256.New()
	if err := writeLenBytes(h, []byte(version)); err != nil {
		return "", err
	}
	if err := writeLen(h, len(files)); err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		name := strings.TrimSpace(file.Name)
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
			return "", fmt.Errorf("lexical: invalid runtime file name %q", file.Name)
		}
		if _, ok := seen[name]; ok {
			return "", fmt.Errorf("lexical: duplicate runtime file %q", name)
		}
		seen[name] = struct{}{}
		if err := writeLenBytes(h, []byte(name)); err != nil {
			return "", err
		}
		if err := writeLenBytes(h, file.Data); err != nil {
			return "", err
		}
	}
	return version + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeLenBytes(h hash.Hash, data []byte) error {
	if err := writeLen(h, len(data)); err != nil {
		return err
	}
	_, err := h.Write(data)
	return err
}

func writeLen(h hash.Hash, n int) error {
	var buf [binary.MaxVarintLen64]byte
	written := binary.PutUvarint(buf[:], uint64(n))
	_, err := h.Write(buf[:written])
	return err
}
