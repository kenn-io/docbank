package pdfproduction

import (
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
)

// pngGrammar preflights the same small container grammar for both independent
// pixel decoders. It forwards bytes only after their framing is validated, uses
// constant memory, and rejects ancillary chunks, interlace, CRC errors and
// bytes following IEND. It does not decode or reconstruct pixels.
type pngGrammar struct {
	r                       io.Reader
	pending                 []byte
	remaining               uint32
	crc                     hash.Hash32
	active, seenIDAT, ended bool
	err                     error
}

func preflightPNG(r io.Reader, width, height int) (*pngGrammar, error) {
	header := make([]byte, 33)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if string(header[:8]) != "\x89PNG\r\n\x1a\n" || binary.BigEndian.Uint32(header[8:12]) != 13 || string(header[12:16]) != "IHDR" || int64(binary.BigEndian.Uint32(header[16:20])) != int64(width) || int64(binary.BigEndian.Uint32(header[20:24])) != int64(height) || header[24] != 8 || (header[25] != 2 && header[25] != 6) || header[26] != 0 || header[27] != 0 || header[28] != 0 || crc32.ChecksumIEEE(header[12:29]) != binary.BigEndian.Uint32(header[29:33]) {
		return nil, errors.New("final PNG requires valid noninterlaced 8-bit RGB/RGBA framing")
	}
	return &pngGrammar{r: r, pending: header, crc: crc32.NewIEEE()}, nil
}

func (g *pngGrammar) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if g.err != nil {
		return 0, g.err
	}
	if len(g.pending) != 0 {
		n := copy(p, g.pending)
		g.pending = g.pending[n:]
		return n, nil
	}
	if g.remaining != 0 {
		n, err := g.r.Read(p[:min(uint64(len(p)), uint64(g.remaining))])
		g.remaining -= uint32(n) //nolint:gosec // The read is bounded to the current chunk length.
		_, _ = g.crc.Write(p[:n])
		g.err = err
		return n, err
	}
	if g.active {
		var sum [4]byte
		if _, g.err = io.ReadFull(g.r, sum[:]); g.err != nil {
			return 0, g.err
		}
		if binary.BigEndian.Uint32(sum[:]) != g.crc.Sum32() {
			g.err = errors.New("final PNG chunk checksum mismatch")
			return 0, g.err
		}
		g.active = false
		g.pending = sum[:]
		return g.Read(p)
	}
	if g.ended {
		var extra [1]byte
		n, err := g.r.Read(extra[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			g.err = errors.New("final PNG has trailing bytes or invalid termination")
		} else {
			g.err = io.EOF
		}
		return 0, g.err
	}
	var header [8]byte
	if _, g.err = io.ReadFull(g.r, header[:]); g.err != nil {
		return 0, g.err
	}
	g.remaining = binary.BigEndian.Uint32(header[:4])
	switch string(header[4:]) {
	case "IDAT":
		g.seenIDAT = true
	case "IEND":
		if !g.seenIDAT || g.remaining != 0 {
			g.err = errors.New("invalid final PNG end chunk")
			return 0, g.err
		}
		g.ended = true
	default:
		g.err = errors.New("final PNG contains unsupported chunks")
		return 0, g.err
	}
	g.crc.Reset()
	_, _ = g.crc.Write(header[4:])
	g.active = true
	g.pending = header[:]
	return g.Read(p)
}
