package bundle

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"go.kenn.io/docbank/internal/canonical"
)

func entryHash(ctx context.Context, r io.ReaderAt, e zipEntry, buffer []byte) (string, error) {
	h := sha256.New()
	crc := crc32.NewIEEE()
	w := &boundedWriter{ctx: ctx, dst: io.MultiWriter(h, crc), limit: e.size}
	n, err := io.CopyBuffer(w, io.NewSectionReader(r, e.offset, e.size), buffer)
	if err != nil {
		return "", err
	}
	if n != e.size || crc.Sum32() != e.crc {
		return "", ErrInvalidArchive
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func expect(r *bufio.Reader, want string) error {
	b := make([]byte, len(want))
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	if string(b) != want {
		return ErrInvalidArchive
	}
	return nil
}

// boundedObject reads one JSON object without letting a hostile document value
// force a manifest-sized allocation before validation.
func boundedObject(r *bufio.Reader) ([]byte, error) {
	b := make([]byte, 0, 1024)
	depth := 0
	quoted, escaped := false, false
	for {
		c, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read bounded bundle object: %w", err)
		}
		b = append(b, c)
		if len(b) > MaxMemberBytes {
			return nil, ErrLimit
		}
		if len(b) == 1 && c != '{' {
			return nil, ErrInvalidArchive
		}
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return b, nil
			}
			if depth < 0 {
				return nil, ErrInvalidArchive
			}
		}
	}
}

// Verify checks the strict ZIP layout, actual complete entry bytes, portable
// plan fingerprint, CSV projection, all checksum lines, and external ZIP hash.
func Verify(ctx context.Context, r io.ReaderAt, size int64, wantFingerprint string) (Receipt, error) {
	if !canonical.IsSHA256Hex(wantFingerprint) {
		return Receipt{}, ErrConflict
	}
	entries, err := readDirectory(r, size)
	if err != nil {
		return Receipt{}, err
	}
	buffer := make([]byte, BufferSize)
	n := len(entries)
	manifest, csvEntry, sums := entries[n-2], entries[n-3], entries[n-1]
	if manifest.name != "bundle.json" || csvEntry.name != "metadata.csv" || sums.name != "SHA256SUMS" || manifest.size > MaxMetadataBytes/2 || csvEntry.size > MaxMetadataBytes/4 || sums.size > MaxMetadataBytes-manifest.size-csvEntry.size {
		return Receipt{}, ErrInvalidArchive
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(r, manifest.offset, manifest.size), BufferSize)
	if err = expect(reader, "{\"plan\":"); err != nil {
		return Receipt{}, err
	}
	raw, err := boundedObject(reader)
	if err != nil {
		return Receipt{}, err
	}
	var plan Plan
	if err = json.Unmarshal(raw, &plan, json.RejectUnknownMembers(true)); err != nil {
		return Receipt{}, err
	}
	if plan.Format != Format || plan.Fingerprint != wantFingerprint || plan.Total < 1 || plan.Total > MaxMembers || plan.RoleEntries != n-3 || plan.RoleEntries > MaxRoles || plan.RoleBytes < 0 || plan.RoleBytes > MaxRoleBytes {
		return Receipt{}, ErrInvalidArchive
	}
	plan.Fingerprint = ""
	header, err := canonical.Marshal(plan)
	if err != nil {
		return Receipt{}, err
	}
	fingerprint := sha256.New()
	_, _ = fingerprint.Write(header)
	_, _ = fingerprint.Write([]byte{'\n'})
	if err = expect(reader, ",\"documents\":["); err != nil {
		return Receipt{}, err
	}
	checksums := bufio.NewReaderSize(io.NewSectionReader(r, sums.offset, sums.size), BufferSize)
	checkLine := func(hash, name string) error { return expect(checksums, fmt.Sprintf("%s  %s\n", hash, name)) }
	csvHash := sha256.New()
	csvWriter := csv.NewWriter(csvHash)
	if err = csvWriter.Write(csvHeader); err != nil {
		return Receipt{}, err
	}
	index, count := 0, 0
	var roleBytes int64
	var previous Member
	for {
		peek, err := reader.Peek(1)
		if err != nil {
			return Receipt{}, fmt.Errorf("read bundle documents: %w", err)
		}
		if peek[0] == ']' {
			break
		}
		if count > 0 {
			if err = expect(reader, ","); err != nil {
				return Receipt{}, err
			}
		}
		raw, err := boundedObject(reader)
		if err != nil {
			return Receipt{}, err
		}
		var d Document
		if err = json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
			return Receipt{}, err
		}
		canonicalBytes, err := canonical.Marshal(d)
		if err != nil {
			return Receipt{}, err
		}
		if !bytes.Equal(raw, canonicalBytes) {
			return Receipt{}, ErrInvalidArchive
		}
		if count >= plan.Total || d.NodeID < 1 || count > 0 && (d.NodeID < previous.NodeID || d.NodeID == previous.NodeID && d.VersionID <= previous.VersionID) {
			return Receipt{}, ErrInvalidArchive
		}
		previous = d.Member
		count++
		_, _ = fingerprint.Write(raw)
		_, _ = fingerprint.Write([]byte{'\n'})
		if err = writeCSVDocument(csvWriter, d); err != nil {
			return Receipt{}, err
		}
		for _, role := range d.Roles {
			if role.Status == "unavailable" {
				if role.Path != "" || role.SHA256 != "" || role.Size != 0 {
					return Receipt{}, ErrInvalidArchive
				}
				continue
			}
			if role.Status != "available" || index >= n-3 || role.Path != entries[index].name || role.Size != entries[index].size || role.Size > MaxRoleBytes-roleBytes {
				return Receipt{}, ErrInvalidArchive
			}
			h, err := entryHash(ctx, r, entries[index], buffer)
			if err != nil {
				return Receipt{}, err
			}
			if h != role.SHA256 {
				return Receipt{}, ErrInvalidArchive
			}
			if err = checkLine(h, role.Path); err != nil {
				return Receipt{}, err
			}
			index++
			roleBytes += role.Size
		}
	}
	if err = expect(reader, "]}\n"); err != nil {
		return Receipt{}, err
	}
	if _, err = reader.ReadByte(); !errors.Is(err, io.EOF) {
		return Receipt{}, ErrInvalidArchive
	}
	if count != plan.Total || index != plan.RoleEntries || roleBytes != plan.RoleBytes || hex.EncodeToString(fingerprint.Sum(nil)) != wantFingerprint {
		return Receipt{}, ErrInvalidArchive
	}
	csvWriter.Flush()
	if err = csvWriter.Error(); err != nil {
		return Receipt{}, err
	}
	for _, entry := range []zipEntry{csvEntry, manifest} {
		h, err := entryHash(ctx, r, entry, buffer)
		if err != nil {
			return Receipt{}, err
		}
		if entry.name == "metadata.csv" && h != hex.EncodeToString(csvHash.Sum(nil)) {
			return Receipt{}, ErrInvalidArchive
		}
		if err = checkLine(h, entry.name); err != nil {
			return Receipt{}, err
		}
	}
	if _, err = checksums.ReadByte(); !errors.Is(err, io.EOF) {
		return Receipt{}, ErrInvalidArchive
	}
	if _, err = entryHash(ctx, r, sums, buffer); err != nil {
		return Receipt{}, err
	}
	whole := sha256.New()
	w := &boundedWriter{ctx: ctx, dst: whole, limit: MaxArchiveBytes}
	if _, err = io.CopyBuffer(w, io.NewSectionReader(r, 0, size), buffer); err != nil {
		return Receipt{}, err
	}
	if w.n != size {
		return Receipt{}, ErrInvalidArchive
	}
	return Receipt{Format: Format, PlanFingerprint: wantFingerprint, SHA256: hex.EncodeToString(whole.Sum(nil)), Size: size, Entries: n}, nil
}
