package bundle

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

type Walk func(func(Document) error) error
type Open func(Role) (io.ReadCloser, error)

func Fingerprint(plan Plan, walk Walk) (string, error) {
	plan.Fingerprint = ""
	raw, err := canonical.Marshal(plan)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write(raw)
	_, _ = h.Write([]byte{'\n'})
	err = walk(func(d Document) error {
		raw, err := canonical.Marshal(d)
		if err != nil {
			return err
		}
		if len(raw) > MaxMemberBytes {
			return ErrLimit
		}
		_, _ = h.Write(raw)
		_, _ = h.Write([]byte{'\n'})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

type boundedWriter struct {
	ctx      context.Context
	dst      io.Writer
	n, limit int64
	hash     hash.Hash
	check    func() error
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.check != nil {
		if err := w.check(); err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > w.limit-w.n {
		return 0, ErrLimit
	}
	n, err := w.dst.Write(p)
	if w.hash != nil {
		_, _ = w.hash.Write(p[:n])
	}
	w.n += int64(n)
	return n, err
}

func safePath(path string) bool {
	if len(path) < 1 || len(path) > 240 || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") {
		return false
	}
	for _, c := range path {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '/' && c != '.' && c != '-' && c != '_' {
			return false
		}
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") {
			return false
		}
		base, _, _ := strings.Cut(part, ".")
		switch base {
		case "con", "prn", "aux", "nul", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
			return false
		}
	}
	return true
}

func csvCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) || strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") {
		return "'" + value
	}
	return value
}

var csvHeader = []string{"node_id", "version_id", "source_sha256", "name", "path", "role", "status", "role_path", "sha256", "size"}

func writeCSVDocument(w *csv.Writer, d Document) error {
	for _, r := range d.Roles {
		row := []string{strconv.FormatInt(d.NodeID, 10), d.VersionID, d.SHA256, csvCell(d.Name), csvCell(d.Path), r.Role, r.Status, r.Path, r.SHA256, strconv.FormatInt(r.Size, 10)}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// CSVDocumentBytes measures the exact spreadsheet projection without buffering it.
func CSVDocumentBytes(d Document) (int64, error) {
	counter := &boundedWriter{ctx: context.Background(), dst: io.Discard, limit: MaxMetadataBytes / 4}
	writer := csv.NewWriter(counter)
	err := writeCSVDocument(writer, d)
	writer.Flush()
	if err != nil {
		return 0, err
	}
	return counter.n, writer.Error()
}

// Write streams sources into a deterministic Store-only ZIP64 archive. Source
// streams must finish with their expected digest; successful writing is followed
// by independent readback of the actual archive before a receipt is returned.
func Write(ctx context.Context, file *os.File, plan Plan, walk Walk, open Open, progress func(int, int64) error) (Receipt, error) {
	if plan.Format != Format || plan.Total < 1 || plan.Total > MaxMembers || plan.RoleEntries > MaxRoles || plan.RoleBytes > MaxRoleBytes {
		return Receipt{}, ErrLimit
	}
	fingerprint, err := Fingerprint(plan, walk)
	if err != nil {
		return Receipt{}, err
	}
	if fingerprint != plan.Fingerprint {
		return Receipt{}, ErrConflict
	}
	if err = file.Truncate(0); err != nil {
		return Receipt{}, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return Receipt{}, err
	}
	// Disk-backed checksum text avoids retaining all entry metadata in memory.
	sums, err := os.CreateTemp(filepath.Dir(file.Name()), ".export-sums-*")
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = sums.Close(); _ = os.Remove(sums.Name()) }()
	archive := &boundedWriter{ctx: ctx, dst: file, limit: MaxArchiveBytes}
	z := zip.NewWriter(archive)
	closed := false
	defer func() {
		if !closed {
			_ = z.Close()
		}
	}()
	entries := 0
	buffer := make([]byte, BufferSize)
	var roleBytes, metadataBytes int64
	add := func(name string, limit int64, write func(io.Writer) error) (string, int64, error) {
		if !safePath(name) && name != "SHA256SUMS" {
			return "", 0, ErrInvalidArchive
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Fixed DOS time avoids optional timestamp extra fields in the strict archive profile.
		header.SetMode(0600)
		dst, err := z.CreateHeader(header)
		if err != nil {
			return "", 0, fmt.Errorf("write ZIP entry header: %w", err)
		}
		h := sha256.New()
		w := &boundedWriter{ctx: ctx, dst: dst, limit: limit, hash: h}
		if err = write(w); err != nil {
			return "", 0, err
		}
		entries++
		return hex.EncodeToString(h.Sum(nil)), w.n, nil
	}
	count := 0
	var previous Member
	err = walk(func(d Document) error {
		if count > 0 && (d.NodeID < previous.NodeID || d.NodeID == previous.NodeID && d.VersionID <= previous.VersionID) {
			return ErrConflict
		}
		previous = d.Member
		count++
		for _, r := range d.Roles {
			if r.Status == "unavailable" {
				if r.Path != "" || r.SHA256 != "" || r.Size != 0 {
					return ErrConflict
				}
				continue
			}
			if r.Status != "available" || !canonical.IsSHA256Hex(r.SHA256) || r.Size < 0 || r.Size > MaxRoleBytes-roleBytes || entries >= MaxRoles {
				return ErrLimit
			}
			h, n, err := add(r.Path, r.Size, func(w io.Writer) error {
				src, err := open(r)
				if err != nil {
					return err
				}
				_, copyErr := io.CopyBuffer(w, src, buffer)
				closeErr := src.Close()
				if copyErr != nil {
					return copyErr
				}
				return closeErr
			})
			if err != nil {
				return err
			}
			if h != r.SHA256 || n != r.Size {
				return ErrInvalidArchive
			}
			roleBytes += n
			if _, err = fmt.Fprintf(sums, "%s  %s\n", h, r.Path); err != nil {
				return fmt.Errorf("write role checksum: %w", err)
			}
			if progress != nil {
				if err = progress(entries, roleBytes); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if count != plan.Total || entries != plan.RoleEntries || roleBytes != plan.RoleBytes {
		return Receipt{}, ErrConflict
	}
	h, n, err := add("metadata.csv", MaxMetadataBytes/4, func(dst io.Writer) error {
		w := csv.NewWriter(dst)
		if err := w.Write(csvHeader); err != nil {
			return err
		}
		err := walk(func(d Document) error { return writeCSVDocument(w, d) })
		w.Flush()
		if err != nil {
			return err
		}
		return w.Error()
	})
	if err != nil {
		return Receipt{}, err
	}
	metadataBytes += n
	if _, err = fmt.Fprintf(sums, "%s  metadata.csv\n", h); err != nil {
		return Receipt{}, fmt.Errorf("write metadata checksum: %w", err)
	}
	h, n, err = add("bundle.json", MaxMetadataBytes/2, func(dst io.Writer) error {
		header, err := canonical.Marshal(plan)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(dst, "{\"plan\":%s,\"documents\":[", header); err != nil {
			return fmt.Errorf("write bundle header: %w", err)
		}
		index := 0
		err = walk(func(d Document) error {
			raw, e := canonical.Marshal(d)
			if e != nil {
				return e
			}
			if len(raw) > MaxMemberBytes {
				return ErrLimit
			}
			if index > 0 {
				if _, e = io.WriteString(dst, ","); e != nil {
					return e
				}
			}
			index++
			_, e = dst.Write(raw)
			return e
		})
		if err != nil {
			return err
		}
		_, err = io.WriteString(dst, "]}\n")
		return err
	})
	if err != nil {
		return Receipt{}, err
	}
	metadataBytes += n
	if _, err = fmt.Fprintf(sums, "%s  bundle.json\n", h); err != nil {
		return Receipt{}, fmt.Errorf("write manifest checksum: %w", err)
	}
	if _, err = sums.Seek(0, io.SeekStart); err != nil {
		return Receipt{}, err
	}
	_, _, err = add("SHA256SUMS", MaxMetadataBytes-metadataBytes, func(dst io.Writer) error { _, e := io.CopyBuffer(dst, sums, make([]byte, BufferSize)); return e })
	if err != nil {
		return Receipt{}, err
	}
	if err = z.Close(); err != nil {
		return Receipt{}, fmt.Errorf("finish ZIP directory: %w", err)
	}
	closed = true
	if err = file.Sync(); err != nil {
		return Receipt{}, err
	}
	return Verify(ctx, file, archive.n, plan.Fingerprint)
}
