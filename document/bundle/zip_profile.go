package bundle

import (
	"bytes"
	"encoding/binary"
	"io"
)

var le = binary.LittleEndian

type zipEntry struct {
	name         string
	offset, size int64
	crc          uint32
}

func readAt(r io.ReaderAt, offset int64, n int) ([]byte, error) {
	if offset < 0 || n < 0 {
		return nil, ErrInvalidArchive
	}
	b := make([]byte, n)
	_, err := r.ReadAt(b, offset)
	return b, err
}

// readDirectory recognizes only our contiguous, descriptor-bearing Store ZIP
// profile. All counts and offsets are checked before directory allocation.
//
//nolint:gosec // Every ZIP64 conversion is bounded by the positive 52 GiB archive and 64 MiB directory limits before use.
func readDirectory(r io.ReaderAt, size int64) ([]zipEntry, error) {
	if size < 22 || size > MaxArchiveBytes {
		return nil, ErrLimit
	}
	e, err := readAt(r, size-22, 22)
	if err != nil {
		return nil, err
	}
	if le.Uint32(e) != 0x06054b50 || le.Uint16(e[4:]) != 0 || le.Uint16(e[6:]) != 0 || le.Uint16(e[20:]) != 0 || le.Uint16(e[8:]) != le.Uint16(e[10:]) {
		return nil, ErrInvalidArchive
	}
	count := uint64(le.Uint16(e[10:]))
	directorySize := uint64(le.Uint32(e[12:]))
	directoryOffset := uint64(le.Uint32(e[16:]))
	trailer := size - 22
	zip64 := count == 65535 || directorySize == 0xffffffff || directoryOffset == 0xffffffff
	// A large individual member also requires ZIP64 even when EOCD fields fit.
	if size >= 98 {
		b, readErr := readAt(r, size-42, 4)
		if readErr == nil && le.Uint32(b) == 0x07064b50 {
			zip64 = true
		}
	}
	if zip64 {
		b, err := readAt(r, size-98, 76)
		if err != nil {
			return nil, err
		}
		if le.Uint32(b) != 0x06064b50 || le.Uint64(b[4:]) != 44 || le.Uint16(b[12:]) != 45 || le.Uint16(b[14:]) != 45 || le.Uint32(b[16:]) != 0 || le.Uint32(b[20:]) != 0 || le.Uint64(b[24:]) != le.Uint64(b[32:]) || le.Uint32(b[56:]) != 0x07064b50 || le.Uint32(b[60:]) != 0 || le.Uint64(b[64:]) != uint64(size-98) || le.Uint32(b[72:]) != 1 {
			return nil, ErrInvalidArchive
		}
		count = le.Uint64(b[32:])
		directorySize = le.Uint64(b[40:])
		directoryOffset = le.Uint64(b[48:])
		trailer = size - 98
		if le.Uint16(e[10:]) != uint16(min(count, 65535)) || le.Uint32(e[12:]) != uint32(min(directorySize, 0xffffffff)) || le.Uint32(e[16:]) != uint32(min(directoryOffset, 0xffffffff)) {
			return nil, ErrInvalidArchive
		}
	}
	if count < 3 || count > MaxRoles+3 || directorySize > uint64(MaxDirectoryBytes) || directoryOffset > uint64(trailer) || directorySize != uint64(trailer)-directoryOffset || directorySize < count*46 {
		return nil, ErrLimit
	}
	dir, err := readAt(r, int64(directoryOffset), int(directorySize))
	if err != nil {
		return nil, err
	}
	entries := make([]zipEntry, 0, int(count))
	seen := make(map[string]bool, int(count))
	position := 0
	var next int64
	usedZIP64 := false
	for range int(count) {
		if len(dir)-position < 46 {
			return nil, ErrInvalidArchive
		}
		c := dir[position:]
		if le.Uint32(c) != 0x02014b50 || le.Uint16(c[4:]) != 3<<8|20 || le.Uint16(c[8:]) != 8 || le.Uint16(c[10:]) != 0 || le.Uint16(c[12:]) != 0 || le.Uint16(c[14:]) != 33 || le.Uint16(c[32:]) != 0 || le.Uint32(c[34:]) != 0 || le.Uint32(c[38:]) != 0100600<<16 {
			return nil, ErrInvalidArchive
		}
		nlen, xlen := int(le.Uint16(c[28:])), int(le.Uint16(c[30:]))
		if nlen < 1 || nlen > 240 || xlen > 28 || len(c) < 46+nlen+xlen {
			return nil, ErrInvalidArchive
		}
		name := string(c[46 : 46+nlen])
		if (!safePath(name) && name != "SHA256SUMS") || seen[name] {
			return nil, ErrInvalidArchive
		}
		seen[name] = true
		compressed, uncompressed, offset := uint64(le.Uint32(c[20:])), uint64(le.Uint32(c[24:])), uint64(le.Uint32(c[42:]))
		extra := c[46+nlen : 46+nlen+xlen]
		needs := uncompressed == 0xffffffff || compressed == 0xffffffff || offset == 0xffffffff
		if needs {
			usedZIP64 = true
			if !zip64 || len(extra) < 4 || le.Uint16(extra) != 1 || int(le.Uint16(extra[2:])) != len(extra)-4 || le.Uint16(c[6:]) != 45 {
				return nil, ErrInvalidArchive
			}
			extra = extra[4:]
			for _, v := range []*uint64{&uncompressed, &compressed, &offset} {
				if *v == 0xffffffff {
					if len(extra) < 8 {
						return nil, ErrInvalidArchive
					}
					*v = le.Uint64(extra)
					extra = extra[8:]
				}
			}
			if len(extra) != 0 {
				return nil, ErrInvalidArchive
			}
		} else if xlen != 0 || le.Uint16(c[6:]) != 20 {
			return nil, ErrInvalidArchive
		}
		if compressed != uncompressed || uncompressed > uint64(MaxRoleBytes) || offset != uint64(next) || offset >= directoryOffset {
			return nil, ErrInvalidArchive
		}
		local, err := readAt(r, next, 30+nlen)
		if err != nil {
			return nil, err
		}
		if le.Uint32(local) != 0x04034b50 || le.Uint16(local[4:]) != 20 || le.Uint16(local[6:]) != 8 || le.Uint16(local[8:]) != 0 || le.Uint16(local[10:]) != 0 || le.Uint16(local[12:]) != 33 || le.Uint32(local[14:]) != 0 || le.Uint32(local[18:]) != 0 || le.Uint32(local[22:]) != 0 || int(le.Uint16(local[26:])) != nlen || le.Uint16(local[28:]) != 0 || !bytes.Equal(local[30:], []byte(name)) {
			return nil, ErrInvalidArchive
		}
		data := next + 30 + int64(nlen)
		descriptor := 16
		if uncompressed > 0xffffffff {
			descriptor = 24
		}
		if uint64(data) > directoryOffset || uncompressed > directoryOffset-uint64(data) || uint64(descriptor) > directoryOffset-uint64(data)-uncompressed {
			return nil, ErrInvalidArchive
		}
		d, err := readAt(r, data+int64(uncompressed), descriptor)
		if err != nil {
			return nil, err
		}
		crc := le.Uint32(c[16:])
		if le.Uint32(d) != 0x08074b50 || le.Uint32(d[4:]) != crc {
			return nil, ErrInvalidArchive
		}
		if descriptor == 16 {
			if uint64(le.Uint32(d[8:])) != uncompressed || uint64(le.Uint32(d[12:])) != uncompressed {
				return nil, ErrInvalidArchive
			}
		} else if le.Uint64(d[8:]) != uncompressed || le.Uint64(d[16:]) != uncompressed {
			return nil, ErrInvalidArchive
		}
		entries = append(entries, zipEntry{name: name, offset: data, size: int64(uncompressed), crc: crc})
		next = data + int64(uncompressed) + int64(descriptor)
		position += 46 + nlen + xlen
	}
	if position != len(dir) || next != int64(directoryOffset) {
		return nil, ErrInvalidArchive
	}
	if zip64 && !usedZIP64 && count < 65535 && directoryOffset < 0xffffffff && directorySize < 0xffffffff {
		return nil, ErrInvalidArchive
	}
	return entries, nil
}
