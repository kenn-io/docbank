package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

const (
	MaxRecipientPackageDocuments           = 16
	MaxRecipientPackagePages               = 64
	MaxRecipientPackageBytes         int64 = 512 << 20
	MaxRecipientPackageTextBytes           = 16 << 20
	MaxRecipientPackageMetadataBytes       = 256 << 10
	recipientPackageContract               = "native-pdf-recipient-package/v1"
)

var (
	ErrRecipientPackageInvalid   = errors.New("invalid recipient package input")
	ErrRecipientPackageLimit     = errors.New("recipient package exceeds its limits")
	ErrRecipientPackageIntegrity = errors.New("recipient package contents differ")
)

type RecipientPackageFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type RecipientPackageDocument struct {
	Ordinal                int                  `json:"ordinal"`
	DocumentID             string               `json:"document_id"`
	AllocationID           string               `json:"allocation_id"`
	NamespaceID            string               `json:"namespace_id"`
	ArtifactManifestSHA256 string               `json:"artifact_manifest_sha256"`
	RecipeSHA256           string               `json:"recipe_sha256"`
	ReviewSHA256           string               `json:"review_sha256"`
	PageCount              int                  `json:"page_count"`
	Labels                 []string             `json:"labels"`
	PDF                    RecipientPackageFile `json:"pdf"`
	Text                   RecipientPackageFile `json:"text"`
}

type RecipientPackageReceipt struct {
	Contract      string                     `json:"contract"`
	DocumentCount int                        `json:"document_count"`
	PageCount     int                        `json:"page_count"`
	DATProfile    loadfile.Profile           `json:"dat_profile"`
	OPTProfile    loadfile.Profile           `json:"opt_profile"`
	LoadFile      RecipientPackageFile       `json:"load_file"`
	PageMap       RecipientPackageFile       `json:"page_map"`
	Mapping       RecipientPackageFile       `json:"mapping"`
	Documents     []RecipientPackageDocument `json:"documents"`
}

// ValidateRecipientDocuments admits public metadata before any PDF is opened.
// Text descriptors are pinned separately from each validated review before writing.
func ValidateRecipientDocuments(documents []RecipientPackageDocument) error {
	if len(documents) < 1 || len(documents) > MaxRecipientPackageDocuments {
		return ErrRecipientPackageLimit
	}
	allocations, labels := map[string]bool{}, map[string]bool{}
	var size int64
	pages := 0
	for i, d := range documents {
		if d.Ordinal != i+1 || canonical.ValidateUUIDv4(d.AllocationID) != nil || canonical.ValidateUUIDv4(d.NamespaceID) != nil || allocations[d.AllocationID] {
			return ErrRecipientPackageInvalid
		}
		allocations[d.AllocationID] = true
		if d.PageCount < 1 || d.PageCount > 16 || len(d.Labels) != d.PageCount {
			return ErrRecipientPackageInvalid
		}
		if d.DocumentID != d.Labels[0] || d.PDF.Path != fmt.Sprintf("VOL001/PDF/%06d.pdf", i+1) || d.PDF.Size < 1 {
			return ErrRecipientPackageInvalid
		}
		for _, hash := range []string{d.PDF.SHA256, d.ArtifactManifestSHA256, d.RecipeSHA256, d.ReviewSHA256} {
			if !recipientSHA256(hash) {
				return ErrRecipientPackageInvalid
			}
		}
		for _, label := range d.Labels {
			if len(label) > store.MaxPackageLabelBytes {
				return fmt.Errorf("%w: allocation %s label exceeds the recipient's %d-byte limit", ErrRecipientPackageInvalid, d.AllocationID, store.MaxPackageLabelBytes)
			}
			if len(label) == 0 || !utf8.ValidString(label) || strings.ContainsAny(label, ",\r\n") || labels[label] {
				return fmt.Errorf("%w: allocation %s label %q cannot be preserved in OPT", ErrRecipientPackageInvalid, d.AllocationID, label)
			}
			labels[label] = true
		}
		if d.PDF.Size >= MaxRecipientPackageBytes-size {
			return ErrRecipientPackageLimit
		}
		size += d.PDF.Size
		pages += d.PageCount
		if pages > MaxRecipientPackagePages {
			return ErrRecipientPackageLimit
		}
	}
	return nil
}

func recipientSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// WriteRecipientPackage consumes one fully verified PDF and its reviewed text at
// a time. It closes each source before requesting the next, including on error.
// It never reads a source PDF or validates a review; those are the caller's job.
func WriteRecipientPackage(ctx context.Context, out io.Writer, documents []RecipientPackageDocument,
	source func(context.Context, int) (io.ReadCloser, []byte, error),
) (RecipientPackageReceipt, error) {
	if err := ValidateRecipientDocuments(documents); err != nil {
		return RecipientPackageReceipt{}, err
	}
	receipt := RecipientPackageReceipt{Contract: recipientPackageContract, DocumentCount: len(documents), Documents: slices.Clone(documents)}
	var err error
	receipt.DATProfile, receipt.OPTProfile, err = recipientProfiles()
	if err != nil {
		return receipt, err
	}
	writer := zip.NewWriter(&recipientBoundedWriter{ctx: ctx, out: out, remaining: MaxRecipientPackageBytes})
	for i, d := range receipt.Documents {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		pdf, text, err := source(ctx, i)
		if err != nil {
			return receipt, err
		}
		d, err = writeRecipientDocument(ctx, writer, d, pdf, text)
		if err != nil {
			return receipt, err
		}
		receipt.Documents[i] = d
		receipt.PageCount += d.PageCount
	}
	metadata, err := recipientMetadata(receipt.Documents)
	if err != nil {
		return receipt, err
	}
	receipt.LoadFile = recipientFile("VOL001/DATA.DAT", metadata[0])
	receipt.PageMap = recipientFile("VOL001/DATA.OPT", metadata[1])
	receipt.Mapping = recipientFile("mapping.json", metadata[2])
	encoded, err := canonical.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	metadata = append(metadata, encoded)
	for i, name := range []string{receipt.LoadFile.Path, receipt.PageMap.Path, receipt.Mapping.Path, "receipt.json"} {
		if len(metadata[i]) > MaxRecipientPackageMetadataBytes {
			return receipt, ErrRecipientPackageLimit
		}
		if _, err := writeRecipientEntry(ctx, writer, name, bytes.NewReader(metadata[i])); err != nil {
			return receipt, err
		}
	}
	if err := writer.Close(); err != nil {
		return receipt, fmt.Errorf("finishing recipient ZIP: %w", err)
	}
	return receipt, nil
}

func writeRecipientDocument(ctx context.Context, writer *zip.Writer, d RecipientPackageDocument,
	pdf io.ReadCloser, text []byte,
) (result RecipientPackageDocument, retErr error) {
	defer func() { retErr = errors.Join(retErr, pdf.Close()) }()
	if len(text) > MaxRecipientPackageTextBytes {
		return d, ErrRecipientPackageLimit
	}
	if !utf8.Valid(text) {
		return d, ErrRecipientPackageInvalid
	}
	actual, err := writeRecipientEntry(ctx, writer, d.PDF.Path, io.LimitReader(pdf, d.PDF.Size+1))
	if err != nil {
		return d, err
	}
	if actual != d.PDF {
		return d, ErrRecipientPackageIntegrity
	}
	actual, err = writeRecipientEntry(ctx, writer, fmt.Sprintf("VOL001/TEXT/%06d.txt", d.Ordinal), bytes.NewReader(text))
	if err != nil {
		return d, err
	}
	if actual != d.Text {
		return d, ErrRecipientPackageIntegrity
	}
	return d, nil
}

func writeRecipientEntry(ctx context.Context, writer *zip.Writer, name string, reader io.Reader) (RecipientPackageFile, error) {
	header := &zip.FileHeader{Name: name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // DOS date avoids extended timestamps.
	header.SetMode(0600)
	out, err := writer.CreateHeader(header)
	if err != nil {
		return RecipientPackageFile{}, fmt.Errorf("creating recipient entry %s: %w", name, err)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), &contextReader{ctx: ctx, reader: reader})
	return RecipientPackageFile{Path: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, err
}

func recipientFile(path string, data []byte) RecipientPackageFile {
	return RecipientPackageFile{Path: path, Size: int64(len(data)), SHA256: sha256HexBytes(data)}
}

type recipientBoundedWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
}

func (w *recipientBoundedWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > w.remaining {
		return 0, ErrRecipientPackageLimit
	}
	n, err := w.out.Write(data)
	w.remaining -= int64(n)
	return n, err
}
