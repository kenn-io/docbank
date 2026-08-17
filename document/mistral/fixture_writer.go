package mistral

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/safefileio"
)

const maxSeedBytes = int64(50 << 20)

// ZIP timestamps are encoded directly so fixtures remain byte-for-byte
// reproducible without adding extended timestamp fields. 33 is 1980-01-01 in
// the MS-DOS date representation used by ZIP.
const zipEpochDate = uint16(33)

const openDocumentStyles = `<office:document-styles xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" office:version="1.3"><office:styles/></office:document-styles>`

var nativeSeedFormats = []string{"doc", "ppt", "xls", "numbers", "msg"}

type zipEntry struct {
	name   string
	value  string
	stored bool
}

// FixtureOptions identifies the optional directory containing synthetic native
// format seeds named doc, ppt, xls, numbers, and msg.
type FixtureOptions struct {
	SeedDirectory string
}

// WriteProbeFixtures creates one complete fixture-contract directory without
// credentials or network access. The destination must not exist, and its
// parent must already be private.
func WriteProbeFixtures(ctx context.Context, destination string, options FixtureOptions) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if destination == "" {
		return errors.New("mistral probe fixture destination is required")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("mistral probe fixture destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Mistral probe fixture destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := validatePrivateFixtureDirectory(parent); err != nil {
		return fmt.Errorf("validate Mistral probe fixture destination parent: %w", err)
	}
	var seedRoot *os.Root
	if options.SeedDirectory != "" {
		info, err := os.Lstat(options.SeedDirectory)
		if err != nil {
			return fmt.Errorf("inspect Mistral probe seed directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("mistral probe seed path must be a real directory")
		}
		seedRoot, err = os.OpenRoot(options.SeedDirectory)
		if err != nil {
			return fmt.Errorf("open Mistral probe seed directory: %w", err)
		}
		defer func() {
			if seedRoot != nil {
				err = errors.Join(err, seedRoot.Close())
			}
		}()
		openedInfo, err := seedRoot.Lstat(".")
		if err != nil || !os.SameFile(info, openedInfo) {
			return errors.New("mistral probe seed directory changed while opening")
		}
	}

	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+"-tmp-*")
	if err != nil {
		return fmt.Errorf("create Mistral probe fixture directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(temporary))
		}
	}()
	if err := safefileio.EnsurePrivateDir(temporary); err != nil {
		return fmt.Errorf("secure Mistral probe fixture directory: %w", err)
	}
	missing, err := buildProbeFixtureDirectory(ctx, temporary, seedRoot)
	if err != nil {
		return err
	}
	if seedRoot != nil {
		if err := seedRoot.Close(); err != nil {
			return fmt.Errorf("close Mistral probe seed directory: %w", err)
		}
		seedRoot = nil
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"fixture matrix remains incomplete: supply synthetic native seeds named %s through FixtureOptions.SeedDirectory",
			strings.Join(missing, ", "),
		)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("mistral probe fixture destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Mistral probe fixture destination: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("publish Mistral probe fixtures: %w", err)
	}
	published = true
	return nil
}

func buildProbeFixtureDirectory(ctx context.Context, directory string, seedRoot *os.Root) ([]string, error) {
	missing := make([]string, 0, len(nativeSeedFormats))
	for _, candidate := range candidateFormats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target := filepath.Join(directory, candidate.ID)
		if slices.Contains(nativeSeedFormats, candidate.ID) {
			if seedRoot == nil {
				missing = append(missing, candidate.ID)
				continue
			}
			if _, err := seedRoot.Lstat(candidate.ID); errors.Is(err, os.ErrNotExist) {
				missing = append(missing, candidate.ID)
				continue
			} else if err != nil {
				return nil, fmt.Errorf("inspect fixture seed %q: %w", candidate.ID, err)
			}
			if err := copyFixture(ctx, seedRoot, candidate.ID, target); err != nil {
				return nil, fmt.Errorf("copy fixture seed %q: %w", candidate.ID, err)
			}
		} else {
			content, generated, err := generatedFixture(candidate.ID)
			if err != nil {
				return nil, fmt.Errorf("generate fixture %q: %w", candidate.ID, err)
			}
			if !generated {
				return nil, fmt.Errorf("fixture generator omitted %q", candidate.ID)
			}
			if err := writeNewPrivateFile(target, bytes.NewReader(content), int64(len(content))); err != nil {
				return nil, fmt.Errorf("write fixture %q: %w", candidate.ID, err)
			}
		}
		if err := validateFixture(target, candidate); err != nil {
			return nil, fmt.Errorf("validate fixture %q: %w", candidate.ID, err)
		}
	}
	slices.Sort(missing)
	return missing, nil
}

func copyFixture(ctx context.Context, root *os.Root, name, target string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxSeedBytes {
		return errors.New("fixture seed must be a bounded regular non-symlink file")
	}
	file, err := safefileio.OpenCurrentUserFile(filepath.Join(root.Name(), name))
	if err != nil {
		return fmt.Errorf("open fixture seed without following links: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return errors.New("fixture seed changed while opening")
	}
	return writeNewPrivateFile(target, io.LimitReader(&contextReader{ctx: ctx, reader: file}, maxSeedBytes+1), info.Size())
}

func writeNewPrivateFile(target string, reader io.Reader, expectedSize int64) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- target is a fixed candidate ID under an operator-selected directory.
	if err != nil {
		return err
	}
	if err := secureCreatedFile(file); err != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return fmt.Errorf("secure fixture file: %w", err)
	}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != expectedSize {
		_ = os.Remove(target)
		var sizeErr error
		if written != expectedSize {
			sizeErr = fmt.Errorf("fixture write size %d, want %d", written, expectedSize)
		}
		return errors.Join(copyErr, closeErr, sizeErr)
	}
	return nil
}

func validateFixture(path string, candidate CandidateFormat) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxSeedBytes {
		return errors.New("fixture must be a bounded regular non-symlink file")
	}
	file, err := openPrivateFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return errors.New("fixture changed while opening")
	}
	detected, err := DetectFormat(file, info.Size(), candidate.MediaType)
	if err != nil {
		return err
	}
	if detected.ID != candidate.ID {
		return fmt.Errorf("detected %q, want %q", detected.ID, candidate.ID)
	}
	return nil
}

func generatedFixture(id string) ([]byte, bool, error) {
	sentinel, err := ProbeFixtureSentinel(id)
	if err != nil {
		return nil, false, err
	}
	xmlSentinel := escapeXML(sentinel)
	switch id {
	case "pdf":
		return pdfFixture(sentinel), true, nil
	case "docx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
			{name: "word/document.xml", value: `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + xmlSentinel + `</w:t></w:r></w:p><w:sectPr/></w:body></w:document>`},
		})
	case "odt":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/vnd.oasis.opendocument.text", stored: true},
			{name: "META-INF/manifest.xml", value: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/><manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
			{name: "content.xml", value: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.3"><office:body><office:text><text:p>` + xmlSentinel + `</text:p></office:text></office:body></office:document-content>`},
			{name: "styles.xml", value: openDocumentStyles},
		})
	case "rtf":
		return []byte(`{\rtf1\ansi ` + sentinel + `}`), true, nil
	case "pptx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`},
			{name: "ppt/presentation.xml", value: `<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst><p:sldSz cx="9144000" cy="6858000"/></p:presentation>`},
			{name: "ppt/_rels/presentation.xml.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`},
			{name: "ppt/slides/slide1.xml", value: `<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Probe"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:rPr lang="en-US"/><a:t>` + xmlSentinel + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`},
		})
	case "xlsx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
			{name: "xl/workbook.xml", value: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Probe" sheetId="1" r:id="rId1"/></sheets></workbook>`},
			{name: "xl/_rels/workbook.xml.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
			{name: "xl/worksheets/sheet1.xml", value: `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>` + xmlSentinel + `</t></is></c></row></sheetData></worksheet>`},
		})
	case "ods":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/vnd.oasis.opendocument.spreadsheet", stored: true},
			{name: "META-INF/manifest.xml", value: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.spreadsheet"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/><manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
			{name: "content.xml", value: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.3"><office:body><office:spreadsheet><table:table table:name="Probe"><table:table-row><table:table-cell office:value-type="string"><text:p>` + xmlSentinel + `</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`},
			{name: "styles.xml", value: openDocumentStyles},
		})
	case "csv":
		return []byte("kind,value\nprobe,\"" + sentinel + "\"\n"), true, nil
	case "epub":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/epub+zip", stored: true},
			{name: "META-INF/container.xml", value: `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
			{name: "OEBPS/content.opf", value: `<?xml version="1.0"?><package version="3.0" unique-identifier="id" xmlns="http://www.idpf.org/2007/opf"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="id">urn:uuid:00000000-0000-0000-0000-000000000001</dc:identifier><dc:title>Probe</dc:title><dc:language>en</dc:language></metadata><manifest><item id="chapter" href="chapter.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
			{name: "OEBPS/chapter.xhtml", value: `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Probe</title></head><body><p>` + xmlSentinel + `</p></body></html>`},
		})
	case "txt":
		return []byte(sentinel + "\n"), true, nil
	case "markdown":
		return []byte("# Probe\n\n" + sentinel + "\n"), true, nil
	case "rst":
		return []byte("Probe\n=====\n\n" + sentinel + "\n"), true, nil
	case "latex":
		return []byte(`\documentclass{article}\begin{document}` + sentinel + `\end{document}`), true, nil
	case "json":
		return []byte(`{"probe":"` + sentinel + `"}`), true, nil
	case "jsonl":
		return []byte(`{"kind":"probe","value":"` + sentinel + `"}` + "\n"), true, nil
	case "xml":
		return []byte(`<probe>` + xmlSentinel + `</probe>`), true, nil
	case "yaml":
		return []byte("---\nprobe: \"" + sentinel + "\"\n"), true, nil
	case "go":
		return []byte("package probe\n\nconst sentinel = \"" + sentinel + "\"\n"), true, nil
	case "python":
		return []byte("sentinel = \"" + sentinel + "\"\n"), true, nil
	case "javascript":
		return []byte("const sentinel = \"" + sentinel + "\";\n"), true, nil
	case "eml":
		return []byte("From: probe@example.test\r\nTo: archive@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic probe\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + sentinel + "\r\n"), true, nil
	case "doc", "ppt", "xls", "numbers", "msg":
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("no fixture policy for %q", id)
	}
}

func zipFixture(entries []zipEntry) ([]byte, bool, error) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:         entry.name,
			Method:       zip.Deflate,
			ModifiedDate: zipEpochDate,
		}
		value := []byte(entry.value)
		if entry.stored {
			header.Method = zip.Store
			header.CRC32 = crc32.ChecksumIEEE(value)
			header.CompressedSize64 = uint64(len(value))
			header.UncompressedSize64 = uint64(len(value))
		}
		var part io.Writer
		var err error
		if entry.stored {
			// CreateRaw avoids a data descriptor and keeps package-mandated
			// first mimetype entries stored with no extra fields.
			part, err = writer.CreateRaw(header)
		} else {
			part, err = writer.CreateHeader(header)
		}
		if err != nil {
			return nil, false, fmt.Errorf("create fixture ZIP entry %q: %w", entry.name, err)
		}
		if _, err := part.Write(value); err != nil {
			return nil, false, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, false, fmt.Errorf("close fixture ZIP: %w", err)
	}
	return output.Bytes(), true, nil
}

func escapeXML(value string) string {
	var output bytes.Buffer
	for _, char := range value {
		switch char {
		case '&':
			output.WriteString("&amp;")
		case '<':
			output.WriteString("&lt;")
		case '>':
			output.WriteString("&gt;")
		default:
			output.WriteRune(char)
		}
	}
	return output.String()
}

func pdfFixture(sentinel string) []byte {
	stream := "BT /F1 12 Tf 72 720 Td (" + strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(sentinel) + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 7 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
