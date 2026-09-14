package transfer

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"

	"go.kenn.io/docbank/internal/canonical"
)

type IntegrityAuthority string

const (
	IntegrityZipReceipt  IntegrityAuthority = "zip_receipt"
	IntegritySumsReceipt IntegrityAuthority = "sums_receipt"
	IntegritySumsOnly    IntegrityAuthority = "sums_only"
)

// ErrUnterminatedLine identifies nonempty data at EOF without the required LF.
var ErrUnterminatedLine = errors.New("transfer: unterminated final line")

type PackageFile struct {
	Name string
	Size int64
}

type PackageReader interface {
	Manifest() ManifestV1
	ManifestSHA256() string
	Records(ctx context.Context, visit func(int, []byte) error) error
	OpenBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error)
	WalkFiles(ctx context.Context, visit func(PackageFile) error) error
	OpenFile(ctx context.Context, name string) (io.ReadCloser, int64, error)
	Close() error
}

// ReadCanonicalLine returns one LF-terminated JSON value without the LF.
func ReadCanonicalLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > MaxRecordLineBytes-len(line) {
			return nil, errors.New("transfer: line limit")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return nil, ErrUnterminatedLine
			}
			return nil, fmt.Errorf("transfer: read line: %w", err)
		}
		if len(line) < 2 || line[len(line)-2] == '\r' {
			return nil, errors.New("transfer: invalid LF framing")
		}
		return line[:len(line)-1], nil
	}
}

// ValidatePackagePath rejects every name outside the exact payload namespace.
func ValidatePackagePath(name string) error {
	if err := validatePortablePath(name); err != nil {
		return err
	}
	valid := name == "transfer.json" || name == "records.jsonl" || name == "SHA256SUMS"
	parts := strings.Split(name, "/")
	if len(parts) == 3 && parts[0] == "blobs" && canonical.IsSHA256Hex(parts[2]) && parts[1] == parts[2][:2] {
		valid = true
	}
	if !valid {
		return errors.New("transfer: undeclared package path")
	}
	return nil
}

type dirReader struct {
	root         *os.Root
	manifest     ManifestV1
	manifestHash string
	closeOnce    sync.Once
	closeErr     error
}

func OpenDirectory(directory string) (PackageReader, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("transfer: cannot open package directory")
	}
	reader := &dirReader{root: root}
	if err := reader.preflight(context.Background()); err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid package directory")
	}
	manifestRaw, err := readPackageFile(context.Background(), reader, "transfer.json", MaxManifestBytes)
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: cannot read package manifest")
	}
	if err := CheckStructuredKeys(manifestRaw); err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid package manifest")
	}
	reader.manifest, reader.manifestHash, err = DecodeManifestV1(manifestRaw)
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid package manifest")
	}
	return reader, nil
}

func (reader *dirReader) Manifest() ManifestV1   { return reader.manifest }
func (reader *dirReader) ManifestSHA256() string { return reader.manifestHash }
func (reader *dirReader) Close() error {
	reader.closeOnce.Do(func() { reader.closeErr = reader.root.Close() })
	return reader.closeErr
}

func (reader *dirReader) Records(ctx context.Context, visit func(int, []byte) error) error {
	return streamRecords(ctx, reader, visit)
}

func (reader *dirReader) OpenBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	if !canonical.IsSHA256Hex(digest) {
		return nil, 0, errors.New("transfer: invalid blob digest")
	}
	return reader.OpenFile(ctx, "blobs/"+digest[:2]+"/"+digest)
}

func (reader *dirReader) WalkFiles(ctx context.Context, visit func(PackageFile) error) error {
	return walkDirectory(ctx, reader.root, visit)
}

func (reader *dirReader) OpenFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := ValidatePackagePath(name); err != nil {
		return nil, 0, err
	}
	before, err := reader.root.Lstat(name)
	if err != nil {
		return nil, 0, fmt.Errorf("transfer: inspect package file: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, 0, errors.New("transfer: package entry is not a regular file")
	}
	file, err := reader.root.Open(name)
	if err != nil {
		return nil, 0, fmt.Errorf("transfer: open package file: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("transfer: inspect opened package file: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, 0, errors.New("transfer: package file changed during open")
	}
	return file, after.Size(), nil
}

func (reader *dirReader) preflight(ctx context.Context) error {
	var required [3]bool
	err := walkDirectory(ctx, reader.root, func(file PackageFile) error {
		switch file.Name {
		case "transfer.json":
			required[0] = true
			if file.Size > MaxManifestBytes {
				return errors.New("transfer: manifest limit")
			}
		case "records.jsonl":
			required[1] = true
		case "SHA256SUMS":
			required[2] = true
			if file.Size > MaxChecksumBytes {
				return errors.New("transfer: checksum file limit")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if required != [3]bool{true, true, true} {
		return errors.New("transfer: required package file missing")
	}
	return nil
}

func walkDirectory(ctx context.Context, root *os.Root, visit func(PackageFile) error) error {
	return walkDirectoryWithLimits(ctx, root, MaxZipEntries, 128, visit)
}

func walkDirectoryWithLimits(ctx context.Context, root *os.Root, entryLimit, batchSize int, visit func(PackageFile) error) error {
	if entryLimit < 0 || batchSize <= 0 {
		return errors.New("transfer: invalid directory enumeration bound")
	}
	var total int64
	var blobs int
	var entries int
	var walk func(string) error
	walk = func(directory string) (result error) {
		before, err := root.Lstat(directory)
		if err != nil || !before.IsDir() {
			return errors.New("transfer: inspect package directory entry")
		}
		opened, err := root.Open(directory)
		if err != nil {
			return errors.New("transfer: open package directory entry")
		}
		defer func() {
			if closeErr := opened.Close(); closeErr != nil {
				result = errors.Join(result, errors.New("transfer: close package directory entry"))
			}
		}()
		after, err := opened.Stat()
		if err != nil || !after.IsDir() || !os.SameFile(before, after) {
			return errors.New("transfer: package directory changed during open")
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, readErr := opened.ReadDir(batchSize)
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries++
				if entries > entryLimit {
					return errors.New("transfer: package entry limit")
				}
				name := entry.Name()
				if directory != "." {
					name = directory + "/" + name
				}
				if entry.IsDir() {
					if !validStructuralDirectory(name) {
						return errors.New("transfer: undeclared package directory")
					}
					if err := walk(name); err != nil {
						return err
					}
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return errors.New("transfer: inspect package entry")
				}
				if !info.Mode().IsRegular() {
					return errors.New("transfer: linked or special package entry")
				}
				if err := ValidatePackagePath(name); err != nil {
					return err
				}
				if info.Size() < 0 || info.Size() > MaxExpandedBytes-total {
					return errors.New("transfer: expanded package limit")
				}
				total += info.Size()
				if strings.HasPrefix(name, "blobs/") {
					blobs++
					if blobs > MaxBlobs || info.Size() > MaxBlobBytes {
						return errors.New("transfer: blob limit")
					}
				}
				if err := visit(PackageFile{Name: name, Size: info.Size()}); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return errors.New("transfer: read package directory entry")
			}
		}
	}
	err := walk(".")
	if err != nil {
		return err
	}
	return nil
}

type zipReader struct {
	files        []*zip.File
	manifest     ManifestV1
	manifestHash string
	closer       io.Closer
	closeOnce    sync.Once
	closeErr     error
}

func OpenZip(source io.ReaderAt, size int64) (PackageReader, error) {
	reader := &zipReader{}
	if closer, ok := source.(io.Closer); ok {
		reader.closer = closer
	}
	if source == nil || size < 0 || size > MaxExpandedBytes+MaxZipDirectoryBytes {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid ZIP size")
	}
	if err := preflightZipDirectory(source, size); err != nil {
		_ = reader.Close()
		return nil, err
	}
	archive, err := zip.NewReader(source, size)
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid ZIP package")
	}
	reader.files = archive.File
	if err := reader.preflight(); err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid ZIP package entries")
	}
	manifestRaw, err := readPackageFile(context.Background(), reader, "transfer.json", MaxManifestBytes)
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: cannot read ZIP manifest")
	}
	if err := CheckStructuredKeys(manifestRaw); err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid package manifest")
	}
	reader.manifest, reader.manifestHash, err = DecodeManifestV1(manifestRaw)
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("transfer: invalid package manifest")
	}
	return reader, nil
}

func (reader *zipReader) Manifest() ManifestV1   { return reader.manifest }
func (reader *zipReader) ManifestSHA256() string { return reader.manifestHash }
func (reader *zipReader) Close() error {
	reader.closeOnce.Do(func() {
		if reader.closer != nil {
			reader.closeErr = reader.closer.Close()
		}
	})
	return reader.closeErr
}

func (reader *zipReader) Records(ctx context.Context, visit func(int, []byte) error) error {
	return streamRecords(ctx, reader, visit)
}

func (reader *zipReader) OpenBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	if !canonical.IsSHA256Hex(digest) {
		return nil, 0, errors.New("transfer: invalid blob digest")
	}
	return reader.OpenFile(ctx, "blobs/"+digest[:2]+"/"+digest)
}

func (reader *zipReader) WalkFiles(ctx context.Context, visit func(PackageFile) error) error {
	for _, file := range reader.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if file.UncompressedSize64 > ^uint64(0)>>1 {
			return errors.New("transfer: ZIP entry size overflow")
		}
		size := int64(file.UncompressedSize64) // #nosec G115 -- bounded by MaxInt64 above.
		if err := visit(PackageFile{Name: file.Name, Size: size}); err != nil {
			return err
		}
	}
	return nil
}

func (reader *zipReader) OpenFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := ValidatePackagePath(name); err != nil {
		return nil, 0, err
	}
	index, found := sort.Find(len(reader.files), func(index int) int {
		return strings.Compare(name, reader.files[index].Name)
	})
	if !found || reader.files[index].FileInfo().IsDir() {
		return nil, 0, fs.ErrNotExist
	}
	file := reader.files[index]
	opened, err := file.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("transfer: open ZIP entry: %w", err)
	}
	if file.UncompressedSize64 > ^uint64(0)>>1 {
		_ = opened.Close()
		return nil, 0, errors.New("transfer: ZIP entry size overflow")
	}
	size := int64(file.UncompressedSize64) // #nosec G115 -- bounded by MaxInt64 above.
	return opened, size, nil
}

func (reader *zipReader) preflight() error {
	sort.Slice(reader.files, func(i, j int) bool { return reader.files[i].Name < reader.files[j].Name })
	var total uint64
	var blobs int
	var required [3]bool
	for index, file := range reader.files {
		if index > 0 && reader.files[index-1].Name == file.Name {
			return errors.New("transfer: duplicate ZIP entry")
		}
		isDirectory := file.FileInfo().IsDir()
		if isDirectory != strings.HasSuffix(file.Name, "/") {
			return errors.New("transfer: ambiguous ZIP directory entry")
		}
		name := strings.TrimSuffix(file.Name, "/")
		if isDirectory {
			if !validStructuralDirectory(name) {
				return errors.New("transfer: undeclared package directory")
			}
			continue
		}
		if !file.Mode().IsRegular() || file.Mode()&fs.ModeType != 0 {
			return errors.New("transfer: linked or special ZIP entry")
		}
		if err := ValidatePackagePath(name); err != nil {
			return err
		}
		if file.UncompressedSize64 > uint64(MaxExpandedBytes)-total {
			return errors.New("transfer: expanded package limit")
		}
		total += file.UncompressedSize64
		switch name {
		case "transfer.json":
			required[0] = true
			if file.UncompressedSize64 > MaxManifestBytes {
				return errors.New("transfer: manifest limit")
			}
		case "records.jsonl":
			required[1] = true
		case "SHA256SUMS":
			required[2] = true
			if file.UncompressedSize64 > MaxChecksumBytes {
				return errors.New("transfer: checksum file limit")
			}
		default:
			blobs++
			if blobs > MaxBlobs || file.UncompressedSize64 > uint64(MaxBlobBytes) {
				return errors.New("transfer: blob limit")
			}
		}
	}
	if required != [3]bool{true, true, true} {
		return errors.New("transfer: required package file missing")
	}
	return nil
}

func streamRecords(ctx context.Context, reader PackageReader, visit func(int, []byte) error) (result error) {
	file, _, err := reader.OpenFile(ctx, "records.jsonl")
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			result = errors.Join(result, errors.New("transfer: close records stream"))
		}
	}()
	buffered := bufio.NewReader(file)
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := ReadCanonicalLine(buffered)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if number > MaxRecordLines {
			return errors.New("transfer: record line limit")
		}
		if err := visit(number, line); err != nil {
			return err
		}
	}
}

func readPackageFile(ctx context.Context, reader PackageReader, name string, limit int) ([]byte, error) {
	file, size, err := reader.OpenFile(ctx, name)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > int64(limit) {
		_ = file.Close()
		return nil, errors.New("transfer: package file limit")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(raw) > limit {
		return nil, errors.New("transfer: package file limit")
	}
	return raw, nil
}

func validatePortablePath(name string) error {
	if name == "" || len(name) > MaxPackagePathBytes || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") {
		return errors.New("transfer: invalid package path")
	}
	for _, char := range []byte(name) {
		if char < 0x20 || char > 0x7e {
			return errors.New("transfer: invalid package path")
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." || part == ".." || len(part) > MaxPackagePathSegmentBytes || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return errors.New("transfer: invalid package path")
		}
	}
	return nil
}

func validStructuralDirectory(name string) bool {
	if err := validatePortablePath(name); err != nil {
		return false
	}
	if name == "blobs" {
		return true
	}
	parts := strings.Split(name, "/")
	return len(parts) == 2 && parts[0] == "blobs" && len(parts[1]) == 2 && isLowerHex(parts[1])
}

func isLowerHex(value string) bool {
	for index := range len(value) {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func preflightZipDirectory(source io.ReaderAt, size int64) error {
	return preflightZipDirectoryWithLimits(source, size, MaxZipEntries, MaxZipDirectoryBytes)
}

func preflightZipDirectoryWithLimits(source io.ReaderAt, size int64, maxEntries int, maxDirectoryBytes int64) error {
	if source == nil || size < 0 || maxEntries < 0 || maxDirectoryBytes < 0 {
		return errors.New("transfer: invalid ZIP directory")
	}
	if size < 22 {
		return errors.New("transfer: invalid ZIP directory")
	}
	archiveSize := uint64(size) // #nosec G115 -- size was checked non-negative by OpenZip.
	eocd, eocdReadOffset, err := locateZipDirectoryEnd(source, size)
	if err != nil {
		return err
	}
	eocdOffset := uint64(eocdReadOffset) // #nosec G115 -- the located EOCD is inside the non-negative archive.
	if binary.LittleEndian.Uint16(eocd[4:]) != 0 || binary.LittleEndian.Uint16(eocd[6:]) != 0 {
		return errors.New("transfer: multi-disk ZIP is unsupported")
	}
	entriesDisk := uint64(binary.LittleEndian.Uint16(eocd[8:]))
	entries := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	directorySize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	directoryEnd := eocdOffset
	if entries == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
		locatorOffset := eocdReadOffset - 20
		if locatorOffset < 0 {
			return errors.New("transfer: missing ZIP64 locator")
		}
		locator := make([]byte, 20)
		if _, err := source.ReadAt(locator, locatorOffset); err != nil || binary.LittleEndian.Uint32(locator) != 0x07064b50 {
			return errors.New("transfer: missing ZIP64 locator")
		}
		if binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return errors.New("transfer: multi-disk ZIP64 is unsupported")
		}
		zip64Offset := binary.LittleEndian.Uint64(locator[8:])
		if archiveSize < 56 || zip64Offset > archiveSize-56 {
			return errors.New("transfer: invalid ZIP64 directory")
		}
		zip64Record := make([]byte, 56)
		zip64ReadOffset := int64(zip64Offset) // #nosec G115 -- bounded by the non-negative archive size above.
		if _, err := source.ReadAt(zip64Record, zip64ReadOffset); err != nil || binary.LittleEndian.Uint32(zip64Record) != 0x06064b50 || binary.LittleEndian.Uint64(zip64Record[4:]) < 44 {
			return errors.New("transfer: invalid ZIP64 directory")
		}
		if binary.LittleEndian.Uint32(zip64Record[16:]) != 0 || binary.LittleEndian.Uint32(zip64Record[20:]) != 0 {
			return errors.New("transfer: multi-disk ZIP64 is unsupported")
		}
		entriesDisk = binary.LittleEndian.Uint64(zip64Record[24:])
		entries = binary.LittleEndian.Uint64(zip64Record[32:])
		directorySize = binary.LittleEndian.Uint64(zip64Record[40:])
		directoryOffset = binary.LittleEndian.Uint64(zip64Record[48:])
		directoryEnd = zip64Offset
	}
	if entriesDisk != entries {
		return errors.New("transfer: inconsistent ZIP entry count")
	}
	if entries > uint64(maxEntries) {
		return errors.New("transfer: ZIP entry limit")
	}
	if directorySize > uint64(maxDirectoryBytes) {
		return errors.New("transfer: ZIP central directory limit")
	}
	if directoryOffset > archiveSize || directorySize > archiveSize-directoryOffset || directoryOffset+directorySize != directoryEnd {
		return errors.New("transfer: invalid ZIP directory bounds")
	}

	const centralHeaderBytes = uint64(46)
	offset := directoryOffset
	var actualEntries uint64
	for offset < directoryEnd {
		if actualEntries >= uint64(maxEntries) || directoryEnd-offset < centralHeaderBytes {
			return errors.New("transfer: ZIP entry limit")
		}
		var header [centralHeaderBytes]byte
		readOffset := int64(offset) // #nosec G115 -- bounded by archive size above.
		if _, err := source.ReadAt(header[:], readOffset); err != nil {
			return errors.New("transfer: cannot read ZIP central directory")
		}
		if binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return errors.New("transfer: invalid ZIP central directory header")
		}
		entryBytes := centralHeaderBytes + uint64(binary.LittleEndian.Uint16(header[28:])) + uint64(binary.LittleEndian.Uint16(header[30:])) + uint64(binary.LittleEndian.Uint16(header[32:]))
		if entryBytes > directoryEnd-offset {
			return errors.New("transfer: invalid ZIP central directory bounds")
		}
		offset += entryBytes
		actualEntries++
	}
	if actualEntries != entries {
		return errors.New("transfer: inconsistent ZIP entry count")
	}
	return nil
}

func locateZipDirectoryEnd(source io.ReaderAt, size int64) ([]byte, int64, error) {
	for attempt, requested := range []int64{1024, 65 * 1024} {
		window := min(size, requested)
		tail := make([]byte, window)
		if _, err := source.ReadAt(tail, size-window); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, errors.New("transfer: cannot read ZIP directory")
		}
		for index := len(tail) - 22; index >= 0; index-- {
			if binary.LittleEndian.Uint32(tail[index:]) != 0x06054b50 {
				continue
			}
			commentBytes := int(binary.LittleEndian.Uint16(tail[index+20:]))
			if index+22+commentBytes > len(tail) {
				break
			}
			return tail[index:], size - window + int64(index), nil
		}
		if attempt == 1 || window == size {
			break
		}
	}
	return nil, 0, errors.New("transfer: invalid ZIP directory")
}
