package epub

import (
	"archive/zip"
	"context"
	"errors"
	"mime"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/internal/epubutil"
)

func admitSpine(ctx context.Context, files []*zip.File, records []epubutil.Package) ([]*zip.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(records) != 1 {
		return nil, unsupported()
	}
	record := records[0]
	if record.XMLName.Local != "package" || record.XMLName.Space != "http://www.idpf.org/2007/opf" || len(record.Spine.Items) == 0 {
		return nil, unsupported()
	}
	entries := make(map[string]*zip.File, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.Name == "META-INF/encryption.xml" {
			return nil, unsupported()
		}
		entries[file.Name] = file
	}
	for _, meta := range record.Metadata.Meta {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if meta.Property == "rendition:layout" && strings.TrimSpace(meta.Value) != "reflowable" || meta.Name == "fixed-layout" && meta.Content != "false" {
			return nil, unsupported()
		}
	}
	manifest := make(map[string]epubutil.Item, len(record.Manifest.Items))
	for _, item := range record.Manifest.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if item.ID == "" {
			return nil, unsupported()
		}
		if _, found := manifest[item.ID]; found {
			return nil, unsupported()
		}
		manifest[item.ID] = item
	}
	var spine []*zip.File
	for _, ref := range record.Spine.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, ok := manifest[ref.IDRef]
		mediaType, _, mediaErr := mime.ParseMediaType(item.MediaType)
		if !ok || mediaErr != nil || !strings.EqualFold(mediaType, "application/xhtml+xml") || strings.Contains(item.Properties, "rendition:layout-pre-paginated") || strings.Contains(ref.Properties, "rendition:layout-pre-paginated") {
			return nil, unsupported()
		}
		directory := path.Dir(record.Path)
		for _, base := range []string{record.Base, record.Manifest.Base, item.Base} {
			if strings.TrimSpace(base) == "" {
				continue
			}
			var err error
			directory, err = epubutil.ResolveArchiveBase(directory, base)
			if err != nil {
				return nil, unsupported()
			}
		}
		href, err := localSpineReference(item.HRef)
		if err != nil {
			return nil, unsupported()
		}
		resource, err := epubutil.ArchivePath(href, directory)
		if err != nil || entries[resource] == nil {
			return nil, unsupported()
		}
		spine = append(spine, entries[resource])
	}
	return spine, nil
}

func localSpineReference(reference string) (string, error) {
	parsed, err := url.Parse(epubutil.StripReferenceWhitespace(reference))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" {
		return "", errors.New("EPUB spine reference is invalid")
	}
	path := parsed.EscapedPath()
	if path == "" {
		return "", errors.New("EPUB spine reference is empty")
	}
	return path, nil
}

// virtualUnits counts complete Markdown without inserting wrapping into it.
func virtualUnits(markdown string) int64 {
	if markdown == "" {
		return 0
	}
	markdown = strings.TrimSuffix(markdown, "\n")
	var lines int64
	for line := range strings.SplitSeq(markdown, "\n") {
		lines += max(1, (int64(utf8.RuneCountInString(line))+79)/80)
	}
	return (lines + 47) / 48
}
