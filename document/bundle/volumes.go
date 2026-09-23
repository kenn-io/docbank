package bundle

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/canonical"
)

const MaxVolumeRoleBytes int64 = 512 << 20
const MaxVolumeRoles = 1000
const volumeOverhead int64 = 1 << 20

func (p Plan) ArchiveEntries() int {
	if p.VolumeLimits != nil {
		return p.Volumes + 3
	}
	return p.RoleEntries + 3
}

func ValidateVolumeLimits(limits *VolumeLimits) error {
	if limits != nil && (limits.RoleBytes < 1 || limits.RoleBytes > MaxVolumeRoleBytes || limits.Roles < 1 || limits.Roles > MaxVolumeRoles) {
		return ErrLimit
	}
	return nil
}

// VolumeCursor assigns every available role a deterministic volume in stream
// order; no source bytes are split and no role is dropped to fit a volume.
type VolumeCursor struct {
	Limits       *VolumeLimits
	Index, Roles int
	Bytes        int64
}

func (v *VolumeCursor) Add(size int64) (int, error) {
	if err := ValidateVolumeLimits(v.Limits); err != nil {
		return 0, err
	}
	if v.Limits == nil {
		return 0, nil
	}
	if size < 0 || size > v.Limits.RoleBytes {
		return 0, ErrLimit
	}
	if v.Index == 0 || v.Roles == v.Limits.Roles || size > v.Limits.RoleBytes-v.Bytes {
		v.Index++
		v.Roles, v.Bytes = 0, 0
	}
	v.Roles++
	v.Bytes += size
	return v.Index, nil
}

type VolumeManifest struct {
	Format          string `json:"format"`
	PlanFingerprint string `json:"plan_fingerprint"`
	Index           int    `json:"index"`
	FirstRole       int    `json:"first_role"`
	Roles           int    `json:"roles"`
	RoleBytes       int64  `json:"role_bytes"`
}

func volumePath(index int) string { return fmt.Sprintf("volumes/%06d.zip", index) }

// addZIPEntry is the common strict Store-only writer for both outer archives
// and bounded child volumes. All contents are streamed under a byte limit.
func addZIPEntry(ctx context.Context, z *zip.Writer, name string, limit int64, write func(io.Writer) error) (string, int64, error) {
	if !safePath(name) && name != "SHA256SUMS" {
		return "", 0, ErrInvalidArchive
	}
	header := &zip.FileHeader{Name: name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Fixed DOS time is required by the strict portable profile.
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
	return hex.EncodeToString(h.Sum(nil)), w.n, nil
}

func copyExportRole(dst io.Writer, role Role, open Open, buffer []byte) error {
	src, err := open(role)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(dst, src, buffer)
	return errors.Join(err, src.Close())
}

type volumeWriter struct {
	ctx       context.Context
	plan      Plan
	file      *os.File
	archive   *boundedWriter
	zip       *zip.Writer
	sums      bytes.Buffer
	manifest  VolumeManifest
	buffer    []byte
	completed int
	emit      func(string, int64, func(io.Writer) error) (string, int64, error)
	outerSums io.Writer
}

func newVolumeWriter(ctx context.Context, file *os.File, plan Plan, emit func(string, int64, func(io.Writer) error) (string, int64, error), sums io.Writer) (*volumeWriter, error) {
	f, err := os.CreateTemp(filepath.Dir(file.Name()), ".export-volume-*")
	if err != nil {
		return nil, err
	}
	return &volumeWriter{ctx: ctx, plan: plan, file: f, buffer: make([]byte, BufferSize), emit: emit, outerSums: sums}, nil
}

func (v *volumeWriter) Close() {
	if v.zip != nil {
		_ = v.zip.Close()
	}
	_ = v.file.Close()
	_ = os.Remove(v.file.Name())
}

func (v *volumeWriter) finish() error {
	if v.zip == nil {
		return nil
	}
	raw, err := canonical.Marshal(v.manifest)
	if err != nil {
		return err
	}
	h, _, err := addZIPEntry(v.ctx, v.zip, "volume.json", MaxMemberBytes, func(w io.Writer) error { _, e := w.Write(raw); return e })
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(&v.sums, "%s  volume.json\n", h)
	_, _, err = addZIPEntry(v.ctx, v.zip, "SHA256SUMS", volumeOverhead/2, func(w io.Writer) error { _, e := w.Write(v.sums.Bytes()); return e })
	if err != nil {
		return err
	}
	if err = v.zip.Close(); err != nil {
		return fmt.Errorf("finish volume ZIP directory: %w", err)
	}
	v.zip = nil
	if _, err = VerifyVolume(v.ctx, v.file, v.archive.n, v.plan.Fingerprint); err != nil {
		return err
	}
	name := volumePath(v.manifest.Index)
	h, _, err = v.emit(name, v.archive.n, func(w io.Writer) error {
		_, e := io.CopyBuffer(w, io.NewSectionReader(v.file, 0, v.archive.n), v.buffer)
		return e
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(v.outerSums, "%s  %s\n", h, name)
	if err != nil {
		return fmt.Errorf("write volume checksum: %w", err)
	}
	return nil
}

func (v *volumeWriter) Add(r Role, open Open) error {
	if r.Volume != v.manifest.Index {
		if r.Volume != v.manifest.Index+1 {
			return ErrConflict
		}
		if err := v.finish(); err != nil {
			return err
		}
		if err := v.file.Truncate(0); err != nil {
			return err
		}
		if _, err := v.file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		v.archive = &boundedWriter{ctx: v.ctx, dst: v.file, limit: v.plan.VolumeLimits.RoleBytes + volumeOverhead}
		v.zip = zip.NewWriter(v.archive)
		v.sums.Reset()
		v.manifest = VolumeManifest{Format: "docbank-volume-v1", PlanFingerprint: v.plan.Fingerprint, Index: r.Volume, FirstRole: v.completed}
	}
	h, size, err := addZIPEntry(v.ctx, v.zip, r.Path, r.Size, func(w io.Writer) error { return copyExportRole(w, r, open, v.buffer) })
	if err != nil {
		return err
	}
	if h != r.SHA256 || size != r.Size {
		return ErrInvalidArchive
	}
	v.manifest.Roles++
	v.manifest.RoleBytes += size
	v.completed++
	_, err = fmt.Fprintf(&v.sums, "%s  %s\n", h, r.Path)
	if err != nil {
		return fmt.Errorf("write role checksum: %w", err)
	}
	return nil
}

// VerifyVolume independently checks a child ZIP and its complete checksum list.
// The parent bundle manifest supplies source/recipe authority and reconciliation.
func VerifyVolume(ctx context.Context, r io.ReaderAt, size int64, parentFingerprint string) (VolumeManifest, error) {
	var out VolumeManifest
	if !canonical.IsSHA256Hex(parentFingerprint) || size < 1 || size > MaxVolumeRoleBytes+volumeOverhead {
		return out, ErrLimit
	}
	entries, err := readDirectory(r, size)
	if err != nil {
		return out, err
	}
	n := len(entries)
	if n < 3 || n > MaxVolumeRoles+2 || entries[n-2].name != "volume.json" || entries[n-2].size > MaxMemberBytes || entries[n-1].name != "SHA256SUMS" || entries[n-1].size > volumeOverhead/2 {
		return out, ErrInvalidArchive
	}
	raw := make([]byte, entries[n-2].size)
	if _, err = io.ReadFull(io.NewSectionReader(r, entries[n-2].offset, entries[n-2].size), raw); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)); err != nil {
		return out, err
	}
	if out.Format != "docbank-volume-v1" || out.PlanFingerprint != parentFingerprint || out.Index < 1 || out.Index > MaxRoles || out.FirstRole < 0 || out.FirstRole > MaxRoles-out.Roles || out.Roles != n-2 || out.RoleBytes < 0 || out.RoleBytes > MaxVolumeRoleBytes {
		return out, ErrInvalidArchive
	}
	sums := bufio.NewReader(io.NewSectionReader(r, entries[n-1].offset, entries[n-1].size))
	buffer := make([]byte, BufferSize)
	var roleBytes int64
	for i, entry := range entries {
		h, err := entryHash(ctx, r, entry, buffer)
		if err != nil {
			return out, err
		}
		if i < n-1 {
			if err = expect(sums, fmt.Sprintf("%s  %s\n", h, entry.name)); err != nil {
				return out, err
			}
		}
		if i < n-2 {
			roleBytes += entry.size
		}
	}
	if _, err = sums.ReadByte(); !errors.Is(err, io.EOF) || roleBytes != out.RoleBytes {
		return out, ErrInvalidArchive
	}
	return out, nil
}

type volumeReader struct {
	ctx             context.Context
	reader          io.ReaderAt
	entries         []zipEntry
	plan            Plan
	fingerprint     string
	checkLine       func(string, string) error
	index, consumed int
	manifest        VolumeManifest
	volume          *io.SectionReader
	files           []zipEntry
	buffer          []byte
}

func (v *volumeReader) Add(role Role, ordinal int) error {
	if role.Volume != v.index {
		if role.Volume != v.index+1 || v.consumed != v.manifest.Roles || v.index >= v.plan.Volumes {
			return ErrInvalidArchive
		}
		entry := v.entries[v.index]
		if entry.name != volumePath(role.Volume) || entry.size > v.plan.VolumeLimits.RoleBytes+volumeOverhead {
			return ErrInvalidArchive
		}
		v.volume = io.NewSectionReader(v.reader, entry.offset, entry.size)
		manifest, err := VerifyVolume(v.ctx, v.volume, entry.size, v.fingerprint)
		if err != nil {
			return err
		}
		if manifest.Index != role.Volume || manifest.FirstRole != ordinal || manifest.RoleBytes > v.plan.VolumeLimits.RoleBytes || manifest.Roles > v.plan.VolumeLimits.Roles {
			return ErrInvalidArchive
		}
		v.files, err = readDirectory(v.volume, entry.size)
		if err != nil {
			return err
		}
		h, err := entryHash(v.ctx, v.reader, entry, v.buffer)
		if err != nil {
			return err
		}
		if err = v.checkLine(h, entry.name); err != nil {
			return err
		}
		v.manifest, v.index, v.consumed = manifest, role.Volume, 0
	}
	if v.consumed >= v.manifest.Roles {
		return ErrInvalidArchive
	}
	entry := v.files[v.consumed]
	if role.Path != entry.name || role.Size != entry.size {
		return ErrInvalidArchive
	}
	h, err := entryHash(v.ctx, v.volume, entry, v.buffer)
	if err != nil {
		return err
	}
	if h != role.SHA256 {
		return ErrInvalidArchive
	}
	v.consumed++
	return nil
}

func (v *volumeReader) Finish() error {
	if v.index != v.plan.Volumes || v.consumed != v.manifest.Roles {
		return ErrInvalidArchive
	}
	return nil
}
