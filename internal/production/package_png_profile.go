package production

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
)

var productionPNGSignature = []byte{137, 80, 78, 71, 13, 10, 26, 10}

// Production pages are encoded by Go's png.Encoder from rendered color pixels.
// Its output has IHDR, contiguous IDAT chunks, then IEND. Optional PNG chunks
// are outside the recipient projection and can carry undisclosed text.
func verifyRecipientPNGChunks(ctx context.Context, file *os.File, entry *zip.File, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dataOffset, err := entry.DataOffset()
	if err != nil || dataOffset < 0 || size < 8 || dataOffset > math.MaxInt64-size {
		return ErrRecipientArchive
	}
	var signature [8]byte
	if _, err := file.ReadAt(signature[:], dataOffset); err != nil ||
		!bytes.Equal(signature[:], productionPNGSignature) {
		return ErrRecipientArchive
	}
	position, state := int64(8), 0
	for chunks := 0; position < size && chunks < 100_000; chunks++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size-position < 12 {
			return ErrRecipientArchive
		}
		var header [8]byte
		if _, err := file.ReadAt(header[:], dataOffset+position); err != nil {
			return ErrRecipientArchive
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		if length > size-position-12 {
			return ErrRecipientArchive
		}
		switch string(header[4:]) {
		case "IHDR":
			if state != 0 || length != 13 {
				return ErrRecipientArchive
			}
			state = 1
		case "IDAT":
			if state != 1 && state != 2 {
				return ErrRecipientArchive
			}
			state = 2
		case "IEND":
			if state != 2 || length != 0 || position+12 != size {
				return ErrRecipientArchive
			}
			return nil
		default:
			return ErrRecipientArchive
		}
		position += 12 + length
	}
	return ErrRecipientArchive
}
