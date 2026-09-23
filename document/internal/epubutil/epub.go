// Package epubutil owns bounded EPUB package reads and archive reference resolution.
package epubutil

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Package preserves manifest declarations and spine occurrences before admission.
type Package struct {
	Path     string   `xml:"-"`
	XMLName  xml.Name `xml:""`
	Base     string   `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Metadata struct {
		Meta []Meta `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Base  string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
		Items []Item `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Items []Itemref `xml:"itemref"`
	} `xml:"spine"`
}

// Meta preserves EPUB layout declarations.
type Meta struct {
	Property string `xml:"property,attr"`
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Value    string `xml:",chardata"`
}

// Item preserves a manifest declaration, including duplicate IDs.
type Item struct {
	ID         string `xml:"id,attr"`
	HRef       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Base       string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Properties string `xml:"properties,attr"`
}

// Itemref is one occurrence, including repeats and non-linear entries.
type Itemref struct {
	IDRef      string `xml:"idref,attr"`
	Linear     string `xml:"linear,attr"`
	Properties string `xml:"properties,attr"`
}

// ReadPackages reads every declared rootfile in order under the caller's entry limit.
func ReadPackages(files []*zip.File, limit int64) ([]Package, error) {
	return ReadPackagesContext(context.Background(), files, limit)
}

// ReadPackagesContext reads package metadata while observing cancellation.
func ReadPackagesContext(ctx context.Context, files []*zip.File, limit int64) ([]Package, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries := make(map[string]*zip.File, len(files))
	for _, file := range files {
		if entries[file.Name] == nil {
			entries[file.Name] = file
		}
	}
	container := entries["META-INF/container.xml"]
	if container == nil {
		return nil, errors.New("EPUB container document is missing")
	}
	body, err := ReadZIPEntryContext(ctx, container, limit)
	if err != nil {
		return nil, err
	}
	if err := validateMetadataXMLContext(ctx, body); err != nil {
		return nil, err
	}
	var containerDocument struct {
		XMLName   xml.Name `xml:"container"`
		Rootfiles struct {
			Items []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := decodeXMLContext(ctx, body, &containerDocument); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("EPUB container document is invalid")
	}
	if containerDocument.XMLName.Space != "urn:oasis:names:tc:opendocument:xmlns:container" || len(containerDocument.Rootfiles.Items) == 0 {
		return nil, errors.New("EPUB container document is invalid")
	}
	var records []Package
	parsed := make(map[string]Package)
	for _, rootfile := range containerDocument.Rootfiles.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		packagePath, err := ArchivePath(rootfile.FullPath, "")
		if err != nil {
			return nil, err
		}
		if record, exists := parsed[packagePath]; exists {
			records = append(records, record)
			continue
		}
		file := entries[packagePath]
		if file == nil {
			return nil, errors.New("EPUB package document is missing")
		}
		body, err := ReadZIPEntryContext(ctx, file, limit)
		if err != nil {
			return nil, err
		}
		if err := validateMetadataXMLContext(ctx, body); err != nil {
			return nil, err
		}
		var record Package
		if err := decodeXMLContext(ctx, body, &record); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, errors.New("EPUB package document is invalid")
		}
		record.Path = packagePath
		parsed[packagePath] = record
		records = append(records, record)
	}
	return records, nil
}

const (
	maxMetadataXMLAttributes = 1 << 16
	maxMetadataXMLElements   = 100_000
)

func validateMetadataXMLContext(ctx context.Context, body []byte) error {
	checkContext := func(index int) error {
		if index&1023 == 0 {
			return ctx.Err()
		}
		return nil
	}
	elements := 0
	for index := 0; index < len(body); {
		if err := checkContext(index); err != nil {
			return err
		}
		if body[index] != '<' || index+1 >= len(body) {
			index++
			continue
		}
		if bytes.HasPrefix(body[index:], []byte("<!--")) {
			index += len("<!--")
			for index+2 < len(body) && !bytes.Equal(body[index:index+3], []byte("-->")) {
				if err := checkContext(index); err != nil {
					return err
				}
				index++
			}
			index += min(3, len(body)-index)
			continue
		}
		if bytes.HasPrefix(body[index:], []byte("<![CDATA[")) {
			index += len("<![CDATA[")
			for index+2 < len(body) && !bytes.Equal(body[index:index+3], []byte("]]>")) {
				if err := checkContext(index); err != nil {
					return err
				}
				index++
			}
			index += min(3, len(body)-index)
			continue
		}
		if body[index+1] == '?' {
			index += 2
			for index+1 < len(body) && (body[index] != '?' || body[index+1] != '>') {
				if err := checkContext(index); err != nil {
					return err
				}
				index++
			}
			index += min(2, len(body)-index)
			continue
		}
		if body[index+1] == '!' {
			return errors.New("EPUB XML directive is unsupported")
		}
		closing := body[index+1] == '/'
		index++
		if closing {
			for index < len(body) && body[index] != '>' {
				if err := checkContext(index); err != nil {
					return err
				}
				index++
			}
			index += min(1, len(body)-index)
			continue
		}
		elements++
		if elements > maxMetadataXMLElements {
			return errors.New("EPUB XML contains too many elements")
		}
		attributes := 0
		quote := byte(0)
		for index < len(body) {
			if err := checkContext(index); err != nil {
				return err
			}
			character := body[index]
			if quote != 0 {
				if character == quote {
					quote = 0
				}
			} else if character == '\'' || character == '"' {
				quote = character
			} else if character == '=' {
				attributes++
				if attributes > maxMetadataXMLAttributes {
					return errors.New("EPUB XML element has too many attributes")
				}
			} else if character == '>' {
				index++
				break
			}
			index++
		}
	}
	return nil
}

func decodeXMLContext(ctx context.Context, body []byte, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := xml.NewDecoder(contextReader{ctx: ctx, reader: bytes.NewReader(body)}).Decode(target); err != nil {
		return fmt.Errorf("decode EPUB XML: %w", err)
	}
	return nil
}

// ResolveArchiveDir applies directory-versus-document base semantics.
func ResolveArchiveDir(home, base string) string {
	if home == "" {
		return ""
	}
	candidate := NormalizeReferencePath(base)
	if candidate == "" {
		return home
	}
	if strings.Contains(candidate, "://") {
		return ""
	}
	// Rooted bases resolve from the container root.
	origin := home
	if rooted, found := strings.CutPrefix(candidate, "/"); found {
		origin, candidate = ".", rooted
	}
	// Document bases contribute their directory; trailing directory steps stay intact.
	resolved := path.Join(origin, candidate)
	if namesDirectory(candidate) {
		return resolved
	}
	return path.Dir(resolved)
}

// ResolveArchiveBase resolves a local XML base, including a base at archive root.
func ResolveArchiveBase(home, base string) (string, error) {
	candidate := separatorsAsSlashes(StripReferenceWhitespace(base))
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("EPUB XML base is invalid")
	}
	resolved := ResolveArchiveDir(home, candidate)
	if resolved == "" || LeavesArchiveRoot(resolved) {
		return "", errors.New("EPUB XML base is invalid")
	}
	return resolved, nil
}

func namesDirectory(reference string) bool {
	if strings.HasSuffix(reference, "/") {
		return true
	}
	last := path.Base(reference)
	return last == "." || last == ".."
}

// NormalizeReferencePath reduces whitespace, separators, and encoded paths.
func NormalizeReferencePath(value string) string {
	candidate := StripReferenceWhitespace(value)
	if index := strings.IndexAny(candidate, "?#"); index >= 0 {
		candidate = candidate[:index]
	}
	// Non-reference values such as the ODF length "0%" retain their original text.
	if decoded, err := url.PathUnescape(candidate); err == nil {
		candidate = decoded
	}
	return separatorsAsSlashes(candidate)
}

func separatorsAsSlashes(value string) string {
	return strings.ReplaceAll(value, `\`, "/")
}

// StripReferenceWhitespace removes whitespace discarded by resource consumers.
func StripReferenceWhitespace(value string) string {
	candidate := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, value)
	return strings.TrimFunc(candidate, func(r rune) bool { return r <= ' ' })
}

// LeavesArchiveRoot reports whether a cleaned path escapes its container.
func LeavesArchiveRoot(resolved string) bool {
	return resolved == ".." || strings.HasPrefix(resolved, "../")
}

// ReadZIPEntry verifies a complete entry within the caller's byte limit.
func ReadZIPEntry(file *zip.File, limit int64) ([]byte, error) {
	return ReadZIPEntryContext(context.Background(), file, limit)
}

// ReadZIPEntryContext verifies a complete entry while observing cancellation.
func ReadZIPEntryContext(ctx context.Context, file *zip.File, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 0 || file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("ZIP entry exceeds bound")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open ZIP entry: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: reader}, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read ZIP entry: %w", err)
	}
	if int64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, errors.New("ZIP entry exceeds bound")
	}
	return data, nil
}

// NewReaderContext builds a ZIP reader whose entry decompression observes ctx.
func NewReaderContext(ctx context.Context, reader io.ReaderAt, size int64) (*zip.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(contextReaderAt{ctx: ctx, reader: reader}, size)
	if err != nil {
		return nil, fmt.Errorf("open ZIP container: %w", err)
	}
	return archive, nil
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (reader contextReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := reader.reader.ReadAt(buffer, offset)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(p)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

// ManifestBases preserves every intermediate XML-base interpretation.
func ManifestBases(packageDir string, bases ...string) []string {
	directories := []string{packageDir}
	shifted := packageDir
	for _, base := range bases {
		if strings.TrimSpace(base) == "" {
			continue
		}
		shifted = ResolveArchiveDir(shifted, strings.TrimSpace(base))
		if shifted == "" {
			return directories
		}
		if !slices.Contains(directories, shifted) {
			directories = append(directories, shifted)
		}
	}
	return directories
}

// ArchivePath resolves a local EPUB reference and rejects traversal.
func ArchivePath(reference, base string) (string, error) {
	reference = separatorsAsSlashes(StripReferenceWhitespace(reference))
	parsed, err := url.Parse(reference)
	if err != nil || parsed.IsAbs() || parsed.Host != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("EPUB resource path is invalid")
	}
	resource, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil {
		return "", errors.New("EPUB resource path is invalid")
	}
	resource = separatorsAsSlashes(resource)
	// A rooted reference names the container root, independent of the package directory.
	if rooted, found := strings.CutPrefix(resource, "/"); found {
		resource = rooted
	} else if base != "" && base != "." {
		resource = path.Join(base, resource)
	}
	resource = path.Clean(resource)
	if resource == "." || path.IsAbs(resource) || LeavesArchiveRoot(resource) {
		return "", errors.New("EPUB resource path is invalid")
	}
	return resource, nil
}
