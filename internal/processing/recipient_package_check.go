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
	"reflect"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/zipcheck"
)

// VerifyRecipientPackage checks a completed local ZIP against the metadata and
// reviews admitted by its caller. It does not establish producer provenance.
func VerifyRecipientPackage(ctx context.Context, reader io.ReaderAt, size int64, expected RecipientPackageReceipt) error {
	if size <= 0 || size > MaxRecipientPackageBytes {
		return ErrRecipientPackageLimit
	}
	if err := ValidateRecipientDocuments(expected.Documents); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	z, err := zip.NewReader(reader, size)
	if err != nil {
		return recipientZIPError(err)
	}
	encoded, err := canonical.Marshal(expected)
	if err != nil {
		return err
	}
	var inventory []RecipientPackageFile
	for _, d := range expected.Documents {
		inventory = append(inventory, d.PDF, d.Text)
	}
	inventory = append(inventory, expected.LoadFile, expected.PageMap, expected.Mapping, recipientFile("receipt.json", encoded))
	if z.Comment != "" || len(z.File) != len(inventory) {
		return ErrRecipientPackageIntegrity
	}
	metadata := make([][]byte, 4)
	for i, f := range z.File {
		want := inventory[i]
		limit := int64(MaxRecipientPackageMetadataBytes)
		if i < 2*len(expected.Documents) {
			limit = MaxRecipientPackageBytes
			if i%2 == 1 {
				limit = MaxRecipientPackageTextBytes
			}
		}
		if !zipcheck.StoredFile(f) || f.Name != want.Path || want.Size < 0 || want.Size > limit || f.UncompressedSize64 != uint64(want.Size) {
			return ErrRecipientPackageIntegrity
		}
		var buffer bytes.Buffer
		var sink = io.Discard
		if i >= 2*len(expected.Documents) {
			sink = &buffer
		}
		if err := checkRecipientEntry(ctx, f, want, sink); err != nil {
			return err
		}
		if i >= 2*len(expected.Documents) {
			metadata[i-2*len(expected.Documents)] = buffer.Bytes()
		}
	}
	decoded, err := canonical.Decode[RecipientPackageReceipt](metadata[3])
	if err != nil || !reflect.DeepEqual(decoded, expected) {
		return ErrRecipientPackageIntegrity
	}
	return checkRecipientMetadata(expected, metadata)
}

func checkRecipientEntry(ctx context.Context, f *zip.File, want RecipientPackageFile, sink io.Writer) error {
	r, err := f.Open()
	if err != nil {
		return recipientZIPError(err)
	}
	hash := sha256.New()
	size, readErr := io.Copy(io.MultiWriter(sink, hash), &contextReader{ctx: ctx, reader: io.LimitReader(r, want.Size+1)})
	err = errors.Join(readErr, r.Close())
	if err != nil {
		return recipientZIPError(err)
	}
	if size != want.Size || hex.EncodeToString(hash.Sum(nil)) != want.SHA256 {
		return ErrRecipientPackageIntegrity
	}
	return nil
}

func recipientZIPError(err error) error {
	if errors.Is(err, zip.ErrFormat) || errors.Is(err, zip.ErrChecksum) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %w", ErrRecipientPackageIntegrity, err)
	}
	return err
}

func checkRecipientMetadata(receipt RecipientPackageReceipt, data [][]byte) error {
	dat, opt, err := recipientProfiles()
	if err != nil {
		return err
	}
	if receipt.Contract != recipientPackageContract || receipt.DocumentCount != len(receipt.Documents) ||
		!reflect.DeepEqual(receipt.DATProfile, dat) || !reflect.DeepEqual(receipt.OPTProfile, opt) ||
		receipt.LoadFile.Path != "VOL001/DATA.DAT" || receipt.PageMap.Path != "VOL001/DATA.OPT" || receipt.Mapping.Path != "mapping.json" {
		return ErrRecipientPackageIntegrity
	}
	records, diagnostics, err := loadfile.ParseDAT(bytes.NewReader(data[0]), dat)
	if err != nil || len(diagnostics) != 0 || len(records) != len(receipt.Documents) {
		return ErrRecipientPackageIntegrity
	}
	mapping, _, err := loadfile.DecodeMapping(data[2], dat.Columns)
	if err != nil || !reflect.DeepEqual(mapping, recipientMapping(dat.Columns)) {
		return ErrRecipientPackageIntegrity
	}
	// Parsing and applying the supplied mapping checks what the recipient will see.
	remaining := int64(MaxRecipientPackageMetadataBytes)
	diagnostics, err = loadfile.ApplyMapping(records, mapping, dat, func(n int64) error {
		if n > remaining {
			return ErrRecipientPackageLimit
		}
		remaining -= n
		return nil
	})
	if err != nil || len(diagnostics) != 0 {
		return ErrRecipientPackageIntegrity
	}
	pages := 0
	for i, d := range receipt.Documents {
		if d.Text.Path != fmt.Sprintf("VOL001/TEXT/%06d.txt", i+1) || d.Text.Size > MaxRecipientPackageTextBytes || !recipientSHA256(d.Text.SHA256) || records[i].DocID != d.DocumentID {
			return ErrRecipientPackageIntegrity
		}
		var values []string
		for _, f := range records[i].Fields {
			values = append(values, f.Raw)
		}
		if !slices.Equal(values, recipientValues(d)) {
			return ErrRecipientPackageIntegrity
		}
		pages += d.PageCount
	}
	if receipt.PageCount != pages {
		return ErrRecipientPackageIntegrity
	}
	images, diagnostics, err := loadfile.ParseOPT(bytes.NewReader(data[1]), opt)
	if err != nil || len(diagnostics) != 0 || !reflect.DeepEqual(images, recipientImages(receipt.Documents)) {
		return ErrRecipientPackageIntegrity
	}
	return nil
}
