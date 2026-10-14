package processing

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"

	"go.kenn.io/docbank/document"
)

func produceVisualPreviewJPEGDC(ctx context.Context, source io.ReadSeeker, base document.VisualPreviewV1, orientation int, config image.Config) (VisualPreviewProduct, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(fmt.Errorf("seeking visual preview source: %w", err))
	}
	recorder := &visualPreviewReadErrorRecorder{reader: &contextReader{ctx: ctx, reader: source}}
	decoded, err := decodeVisualPreviewJPEGDC(bufio.NewReaderSize(recorder, 64*1024))
	if recorder.err != nil {
		err = recorder.err
	}
	if err != nil {
		return visualPreviewJPEGDecodeResult(base, "the verified JPEG cannot be decoded", err)
	}
	w, h := (config.Width+7)/8, (config.Height+7)/8
	if decoded.Bounds().Dx() != w || decoded.Bounds().Dy() != h {
		return failedVisualPreview(base, "decode_failed", "the JPEG dimensions changed during decoding"), nil
	}
	return encodeVisualPreview(base, decoded, w, h, orientation)
}

type visualJPEGDCComponent struct {
	id, h, v, quant byte
	w, hBlocks      int
	dc              []int16
	precision       int
	quantValue      uint16
	quantLatched    bool
}

type visualJPEGDCHuffman struct {
	counts [16]int
	values []byte
}

type visualJPEGDCDecoder struct {
	reader        *bufio.Reader
	width, height int
	components    []visualJPEGDCComponent
	quant         [4]uint16
	huffman       [2][4]visualJPEGDCHuffman
	progressive   bool
	restart       int
	adobe, rgb    bool
	jfif          bool
	maxH, maxV    int
	bits          byte
	nbits         int
}

func decodeVisualPreviewJPEGDC(reader *bufio.Reader) (image.Image, error) {
	d := visualJPEGDCDecoder{reader: reader}
	var signature [2]byte
	if _, err := io.ReadFull(reader, signature[:]); err != nil {
		return nil, err
	}
	if signature != [2]byte{0xff, 0xd8} {
		return nil, jpeg.FormatError("missing SOI")
	}
	marker, err := readVisualPreviewJPEGMarker(reader)
	for err == nil {
		if marker == 0xd9 {
			return d.image()
		}
		var length [2]byte
		if _, err = io.ReadFull(reader, length[:]); err != nil {
			break
		}
		n := int(binary.BigEndian.Uint16(length[:])) - 2
		if n < 0 {
			return nil, jpeg.FormatError("short marker length")
		}
		payload := make([]byte, n)
		if _, err = io.ReadFull(reader, payload); err != nil {
			break
		}
		switch marker {
		case 0xc0, 0xc1, 0xc2:
			err = d.frame(payload, marker == 0xc2)
		case 0xdb:
			err = d.quantization(payload)
		case 0xc4:
			err = d.tables(payload)
		case 0xdd:
			if len(payload) != 2 {
				err = jpeg.FormatError("bad restart interval")
			} else {
				d.restart = int(binary.BigEndian.Uint16(payload))
			}
		case 0xda:
			err = d.scan(payload)
			if err == nil && d.progressive && payload[len(payload)-3] > 0 {
				marker, err = d.skipAC()
				continue
			}
		case 0xe0:
			if len(payload) >= 14 {
				d.jfif = string(payload[:5]) == "JFIF\x00"
			}
		case 0xee:
			if len(payload) >= 12 && string(payload[:5]) == "Adobe" {
				d.adobe, d.rgb = true, payload[11] == 0
			}
		default:
			if marker < 0xe0 || marker > 0xef && marker != 0xfe {
				err = jpeg.UnsupportedError("JPEG frame or coding mode")
			}
		}
		if err == nil {
			marker, err = readVisualPreviewJPEGMarker(reader)
		}
	}
	return nil, err
}

func (d *visualJPEGDCDecoder) frame(p []byte, progressive bool) error {
	if len(d.components) != 0 || len(p) < 6 || len(p) != 6+3*int(p[5]) {
		return jpeg.FormatError("bad frame header")
	}
	if p[0] != 8 || p[5] != 1 && p[5] != 3 {
		return jpeg.UnsupportedError("JPEG precision or components")
	}
	d.height, d.width = int(binary.BigEndian.Uint16(p[1:3])), int(binary.BigEndian.Uint16(p[3:5]))
	if d.width == 0 || d.height == 0 {
		return jpeg.FormatError("empty frame")
	}
	d.progressive = progressive
	d.components = make([]visualJPEGDCComponent, int(p[5]))
	for i := range d.components {
		c := &d.components[i]
		c.id, c.h, c.v, c.quant = p[6+3*i], p[7+3*i]>>4, p[7+3*i]&15, p[8+3*i]
		c.precision = -1
		if c.h == 0 || c.h > 4 || c.v == 0 || c.v > 4 || c.quant > 3 {
			return jpeg.FormatError("bad component parameters")
		}
		if c.h == 3 || c.v == 3 {
			return jpeg.UnsupportedError("JPEG subsampling")
		}
		d.maxH, d.maxV = max(d.maxH, int(c.h)), max(d.maxV, int(c.v))
		for j := range i {
			if c.id == d.components[j].id {
				return jpeg.FormatError("duplicate component")
			}
		}
	}
	if len(d.components) == 1 {
		d.components[0].h, d.components[0].v = 1, 1
		d.maxH, d.maxV = 1, 1
	}
	for i := range d.components {
		c := &d.components[i]
		// Match sampling validation in go/src/image/jpeg/reader.go, processSOF.
		if d.maxH%int(c.h) != 0 || d.maxV%int(c.v) != 0 {
			return jpeg.UnsupportedError("JPEG subsampling")
		}
		c.w = (d.width*int(c.h) + 8*d.maxH - 1) / (8 * d.maxH)
		c.hBlocks = (d.height*int(c.v) + 8*d.maxV - 1) / (8 * d.maxV)
		c.dc = make([]int16, c.w*c.hBlocks)
	}
	return nil
}

func (d *visualJPEGDCDecoder) quantization(p []byte) error {
	for len(p) > 0 {
		precision, table := p[0]>>4, p[0]&15
		n := 64 * (1 + int(precision))
		if precision > 1 || table > 3 || len(p) < n+1 {
			return jpeg.FormatError("bad quantization table")
		}
		if precision == 0 {
			d.quant[table] = uint16(p[1])
		} else {
			d.quant[table] = binary.BigEndian.Uint16(p[1:3])
		}
		p = p[n+1:]
	}
	return nil
}

func (d *visualJPEGDCDecoder) tables(p []byte) error {
	for len(p) > 0 {
		if len(p) < 17 || p[0]>>4 > 1 || p[0]&15 > 3 {
			return jpeg.FormatError("bad Huffman table")
		}
		t := &d.huffman[p[0]>>4][p[0]&15]
		n, available := 0, 1
		for i := range 16 {
			t.counts[i] = int(p[i+1])
			n += t.counts[i]
			available = available*2 - t.counts[i]
			if available < 0 {
				return jpeg.FormatError("oversubscribed Huffman table")
			}
		}
		if n == 0 || n > 256 || len(p) < 17+n {
			return jpeg.FormatError("bad Huffman values")
		}
		t.values = p[17 : 17+n]
		p = p[17+n:]
	}
	return nil
}

func (d *visualJPEGDCDecoder) readBits(n int) (int32, error) {
	var value int32
	for range n {
		if d.nbits == 0 {
			b, err := d.reader.ReadByte()
			if err != nil {
				return 0, fmt.Errorf("reading JPEG entropy: %w", err)
			}
			if b == 0xff {
				stuffed, err := d.reader.ReadByte()
				if err != nil {
					return 0, fmt.Errorf("reading JPEG stuffing: %w", err)
				}
				if stuffed != 0 {
					return 0, jpeg.FormatError("marker inside entropy data")
				}
			}
			d.bits, d.nbits = b, 8
		}
		d.nbits--
		value = value<<1 | int32(d.bits>>d.nbits&1)
	}
	return value, nil
}

func (d *visualJPEGDCDecoder) symbol(table *visualJPEGDCHuffman) (byte, error) {
	code, first, offset := 0, 0, 0
	for i, count := range table.counts {
		bit, err := d.readBits(1)
		if err != nil {
			return 0, err
		}
		code = code<<1 | int(bit)
		if code >= first && code-first < count {
			return table.values[offset+code-first], nil
		}
		offset += count
		first = (first + count) << 1
		if i == 15 {
			return 0, jpeg.FormatError("invalid Huffman code")
		}
	}
	return 0, jpeg.FormatError("missing Huffman table")
}

func (d *visualJPEGDCDecoder) scan(p []byte) error {
	if len(d.components) == 0 || len(p) < 6 || p[0] == 0 || int(p[0]) > len(d.components) || len(p) != 4+2*int(p[0]) {
		return jpeg.FormatError("bad scan header")
	}
	n := int(p[0])
	ss, se, ah, al := p[len(p)-3], p[len(p)-2], p[len(p)-1]>>4, p[len(p)-1]&15
	if d.progressive {
		if ss > se || se > 63 || ss == 0 && se != 0 || ss > 0 && n != 1 || al > 13 || ah != 0 && ah != al+1 {
			return jpeg.FormatError("bad progressive scan")
		}
	} else if ss != 0 || se != 63 || ah != 0 || al != 0 {
		return jpeg.FormatError("bad sequential scan")
	}
	var indices [3]int
	total := 0
	for i := range n {
		indices[i] = -1
		for j, c := range d.components {
			if c.id == p[1+2*i] {
				indices[i] = j
			}
		}
		if indices[i] < 0 || p[2+2*i]>>4 > 3 || p[2+2*i]&15 > 3 {
			return jpeg.FormatError("bad scan component")
		}
		for j := range i {
			if indices[i] == indices[j] {
				return jpeg.FormatError("duplicate scan component")
			}
		}
		c := &d.components[indices[i]]
		// Later DQT markers must not change a component whose first scan already started.
		if !c.quantLatched {
			c.quantValue, c.quantLatched = d.quant[c.quant], true
		}
		total += int(c.h) * int(c.v)
		if ss == 0 {
			if ah == 0 && c.precision != -1 || ah != 0 && c.precision != int(ah) {
				return jpeg.FormatError("bad DC scan order")
			}
			c.precision = int(al)
		}
	}
	if n > 1 && total > 10 {
		return jpeg.FormatError("too many MCU blocks")
	}
	if ss > 0 {
		return nil
	}
	mw, mh := (d.width+8*d.maxH-1)/(8*d.maxH), (d.height+8*d.maxV-1)/(8*d.maxV)
	if n == 1 {
		mw, mh = d.components[indices[0]].w, d.components[indices[0]].hBlocks
	}
	d.nbits = 0
	var predictors [3]int32
	for mcu := range mw * mh {
		if d.restart > 0 && mcu > 0 && mcu%d.restart == 0 {
			d.nbits = 0
			marker, err := readVisualPreviewJPEGMarker(d.reader)
			if err != nil {
				return err
			}
			if int(marker) != 0xd0+(mcu/d.restart-1)%8 {
				return jpeg.FormatError("wrong restart marker")
			}
			predictors = [3]int32{}
		}
		for i := range n {
			c := &d.components[indices[i]]
			h, v := int(c.h), int(c.v)
			if n == 1 {
				h, v = 1, 1
			}
			for b := range h * v {
				x, y := mcu%mw*h+b%h, mcu/mw*v+b/h
				var dc int32
				if ah != 0 {
					bit, err := d.readBits(1)
					if err != nil {
						return err
					}
					if x < c.w && y < c.hBlocks {
						dc = int32(c.dc[y*c.w+x]) | bit<<al
					}
				} else {
					category, err := d.symbol(&d.huffman[0][p[2+2*i]>>4])
					if err != nil {
						return err
					}
					if category > 11 {
						return jpeg.FormatError("bad DC magnitude")
					}
					diff, err := d.readBits(int(category))
					if err != nil {
						return err
					}
					if category > 0 && diff < 1<<(category-1) {
						diff -= 1<<category - 1
					}
					predictors[i] += diff
					dc = predictors[i] << al
				}
				if dc < -32768 || dc > 32767 {
					return jpeg.FormatError("DC coefficient overflow")
				}
				if x < c.w && y < c.hBlocks {
					c.dc[y*c.w+x] = int16(dc)
				}
				if !d.progressive {
					if err := d.discardAC(&d.huffman[1][p[2+2*i]&15]); err != nil {
						return err
					}
				}
			}
		}
	}
	d.nbits = 0
	return nil
}

func (d *visualJPEGDCDecoder) discardAC(table *visualJPEGDCHuffman) error {
	for k := 1; k < 64; {
		symbol, err := d.symbol(table)
		if err != nil {
			return err
		}
		run, size := int(symbol>>4), int(symbol&15)
		if size == 0 {
			if run == 0 {
				return nil
			}
			if run != 15 {
				return jpeg.FormatError("bad AC run")
			}
			k += 16
		} else {
			k += run + 1
			if size > 10 || k > 64 {
				return jpeg.FormatError("bad AC magnitude or run")
			}
			if _, err := d.readBits(size); err != nil {
				return err
			}
		}
		if k > 64 {
			return jpeg.FormatError("AC run overflow")
		}
	}
	return nil
}

func (d *visualJPEGDCDecoder) skipAC() (byte, error) {
	hasEntropy := false
	for {
		b, err := d.reader.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("skipping JPEG AC scan: %w", err)
		}
		if b != 0xff {
			hasEntropy = true
			continue
		}
		for b == 0xff {
			b, err = d.reader.ReadByte()
			if err != nil {
				return 0, fmt.Errorf("reading JPEG AC marker: %w", err)
			}
		}
		if b == 0 {
			hasEntropy = true
		} else if b < 0xd0 || b > 0xd7 {
			if !hasEntropy {
				return 0, jpeg.FormatError("empty AC scan")
			}
			return b, nil
		}
	}
}

func (d *visualJPEGDCDecoder) image() (image.Image, error) {
	if len(d.components) == 0 {
		return nil, jpeg.FormatError("missing frame")
	}
	for _, c := range d.components {
		if c.precision < 0 || c.quantValue == 0 {
			return nil, jpeg.FormatError("missing DC scan or quantization table")
		}
	}
	bounds := image.Rect(0, 0, (d.width+7)/8, (d.height+7)/8)
	if len(d.components) == 1 {
		gray := image.NewGray(bounds)
		for i := range gray.Pix {
			gray.Pix[i] = d.components[0].sample(i)
		}
		return gray, nil
	}
	// Match JFIF precedence and RGB detection in go/src/image/jpeg/reader.go, isRGB.
	rgb := !d.jfif && (d.adobe && d.rgb || d.components[0].id == 'R' && d.components[1].id == 'G' && d.components[2].id == 'B')
	output := image.NewRGBA(bounds)
	for y := range bounds.Dy() {
		for x := range bounds.Dx() {
			var values [3]byte
			for i, c := range d.components {
				cx, cy := x/(d.maxH/int(c.h)), y/(d.maxV/int(c.v))
				values[i] = c.sample(cy*c.w + cx)
			}
			r, g, b := values[0], values[1], values[2]
			if !rgb {
				r, g, b = color.YCbCrToRGB(r, g, b)
			}
			output.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return output, nil
}

func (c *visualJPEGDCComponent) sample(i int) byte {
	// The DC coefficient gives the block mean without a full inverse transform.
	value := (int64(c.dc[i])*int64(c.quantValue)+4)>>3 + 128
	return byte(max(0, min(255, value)))
}
