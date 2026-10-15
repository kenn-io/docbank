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
)

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
	// lutValue and lutLength decode codes up to 8 bits from the next 8 bits;
	// length zero means a longer code.
	lutValue  [256]byte
	lutLength [256]uint8
}

type visualJPEGDCDecoder struct {
	reader              *bufio.Reader
	width, height       int
	components          []visualJPEGDCComponent
	quant               [4]uint16
	huffman             [2][4]visualJPEGDCHuffman
	progressive         bool
	baseline            bool
	restart             int
	jfif                bool
	adobeTransformValid bool
	adobeTransform      byte
	maxH, maxV          int
	// acc holds nacc unread entropy bits in its low bits.
	acc     uint64
	nacc    int
	stopped bool
	fillErr error
	// pending is a marker that fill consumed while looking ahead.
	pending byte
	ctx     context.Context
	steps   int
}

// check polls for cancellation every 4096 calls so the hot loops stay cheap.
func (d *visualJPEGDCDecoder) check() error {
	d.steps++
	if d.steps%4096 == 0 {
		return d.ctx.Err()
	}
	return nil
}

func decodeVisualPreviewJPEGDC(ctx context.Context, reader *bufio.Reader) (image.Image, error) {
	d := visualJPEGDCDecoder{reader: reader, ctx: ctx}
	if _, err := reader.Discard(2); err != nil {
		return nil, fmt.Errorf("reading JPEG start: %w", err)
	}
	marker, err := d.marker()
	for err == nil {
		if marker == 0xd9 {
			return d.image()
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			marker, err = d.marker()
			continue
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
			err = d.frame(payload, marker)
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
		case 0xe0, 0xee:
			if marker == 0xe0 && len(payload) >= 5 {
				d.jfif = string(payload[:5]) == "JFIF\x00"
			} else if marker == 0xee && len(payload) >= 12 && string(payload[:5]) == "Adobe" {
				d.adobeTransformValid, d.adobeTransform = true, payload[11]
			}
		default:
			if marker < 0xe0 || marker > 0xef && marker != 0xfe {
				err = jpeg.UnsupportedError("JPEG frame or coding mode")
			}
		}
		if err == nil {
			marker, err = d.marker()
		}
	}
	return nil, err
}

func (d *visualJPEGDCDecoder) isRGB() bool {
	if d.jfif {
		return false
	}
	if d.adobeTransformValid && d.adobeTransform == 0 {
		return true
	}
	return d.components[0].id == 'R' && d.components[1].id == 'G' && d.components[2].id == 'B'
}

func (d *visualJPEGDCDecoder) marker() (byte, error) {
	if d.pending != 0 {
		marker := d.pending
		d.pending = 0
		return marker, nil
	}
	for {
		b, err := d.reader.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("reading JPEG marker: %w", err)
		}
		// Match image/jpeg's tolerance for extraneous bytes before markers.
		if b != 0xff {
			continue
		}
		for b == 0xff {
			b, err = d.reader.ReadByte()
			if err != nil {
				return 0, fmt.Errorf("reading JPEG marker fill: %w", err)
			}
		}
		if b != 0 {
			return b, nil
		}
	}
}

func (d *visualJPEGDCDecoder) frame(p []byte, marker byte) error {
	if len(d.components) != 0 || len(p) < 6 {
		return jpeg.FormatError("bad frame header")
	}
	count := int(p[5])
	if (count != 1 && count != 3) || len(p) < 6+3*count {
		return jpeg.FormatError("bad frame header")
	}
	d.height, d.width = int(binary.BigEndian.Uint16(p[1:3])), int(binary.BigEndian.Uint16(p[3:5]))
	d.progressive, d.baseline = marker == 0xc2, marker == 0xc0
	if !d.progressive && d.width*d.height > visualPreviewMaxBaselineDCSourcePixels {
		return errVisualDimensions
	}
	d.components = make([]visualJPEGDCComponent, count)
	for i := range d.components {
		c := &d.components[i]
		c.id, c.h, c.v, c.quant = p[6+3*i], p[7+3*i]>>4, p[7+3*i]&15, p[8+3*i]
		if c.h < 1 || c.h > 4 || c.v < 1 || c.v > 4 || c.quant > 3 {
			d.components = nil
			return jpeg.FormatError("bad frame header")
		}
		c.precision = -1
		d.maxH, d.maxV = max(d.maxH, int(c.h)), max(d.maxV, int(c.v))
	}
	if len(d.components) == 1 {
		d.components[0].h, d.components[0].v = 1, 1
		d.maxH, d.maxV = 1, 1
	}
	for i := range d.components {
		c := &d.components[i]
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
		if len(p) < 17 || p[0]>>4 > 1 || p[0]&15 > 3 || d.baseline && p[0]&15 > 1 {
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
		t.lutLength = [256]uint8{}
		code, k := 0, 0
		for length := 1; length <= 8; length++ {
			for range t.counts[length-1] {
				first := code << (8 - length)
				for i := range 1 << (8 - length) {
					t.lutValue[first+i], t.lutLength[first+i] = t.values[k], uint8(length)
				}
				code++
				k++
			}
			code <<= 1
		}
		p = p[17+n:]
	}
	return nil
}

// fill buffers entropy bytes until a marker, a read error, or a full
// accumulator. A marker found while looking ahead is kept for marker().
func (d *visualJPEGDCDecoder) fill() {
	for !d.stopped && d.nacc <= 56 {
		b, err := d.reader.ReadByte()
		if err != nil {
			d.stopped, d.fillErr = true, err
			return
		}
		if b == 0xff {
			if b, err = d.reader.ReadByte(); err != nil {
				d.stopped, d.fillErr = true, err
				return
			}
			if b != 0 {
				d.stopped = true
				for b == 0xff {
					if b, err = d.reader.ReadByte(); err != nil {
						d.fillErr = err
						return
					}
				}
				// After fill bytes, 0x00 is no marker; marker() keeps scanning.
				d.pending = b
				return
			}
			b = 0xff
		}
		d.acc = d.acc<<8 | uint64(b)
		d.nacc += 8
	}
}

// align drops the fill bits at a restart marker or scan end. Whole bytes still
// buffered precede the next marker, which marker() would also skip.
func (d *visualJPEGDCDecoder) align() error {
	err := d.fillErr
	d.acc, d.nacc, d.stopped, d.fillErr = 0, 0, false, nil
	if err != nil {
		return fmt.Errorf("reading JPEG entropy: %w", err)
	}
	return nil
}

func (d *visualJPEGDCDecoder) readBits(n int) (int32, error) {
	if d.nacc < n {
		d.fill()
		if d.nacc < n {
			if d.fillErr != nil {
				return 0, fmt.Errorf("reading JPEG entropy: %w", d.fillErr)
			}
			return 0, jpeg.FormatError("marker inside entropy data")
		}
	}
	d.nacc -= n
	// n is at most 16, so the constant mask keeps the value in int32 range.
	return int32(d.acc>>d.nacc&0xffff) & (1<<n - 1), nil
}

func (d *visualJPEGDCDecoder) symbol(table *visualJPEGDCHuffman) (byte, error) {
	if d.nacc < 16 {
		d.fill()
	}
	if d.nacc >= 8 {
		next := d.acc >> (d.nacc - 8) & 0xff
		if length := int(table.lutLength[next]); length != 0 {
			d.nacc -= length
			return table.lutValue[next], nil
		}
	}
	code, first, offset := 0, 0, 0
	for _, count := range table.counts {
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
	}
	return 0, jpeg.FormatError("invalid Huffman code")
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
		if d.baseline && (p[2+2*i]>>4 > 1 || p[2+2*i]&15 > 1) {
			return jpeg.FormatError("bad baseline Huffman selector")
		}
		for j := range i {
			if indices[i] == indices[j] {
				return jpeg.FormatError("duplicate scan component")
			}
		}
		c := &d.components[indices[i]]
		// Sequential decoding uses the quantization table defined at the scan.
		if !d.progressive && !c.quantLatched {
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
	var predictors [3]int32
	for mcu := range mw * mh {
		if d.restart > 0 && mcu > 0 && mcu%d.restart == 0 {
			if err := d.align(); err != nil {
				return err
			}
			marker, err := d.marker()
			if err != nil {
				return err
			}
			if int(marker) != 0xd0+(mcu/d.restart-1)%8 {
				return jpeg.FormatError("wrong restart marker")
			}
			predictors = [3]int32{}
		}
		if err := d.check(); err != nil {
			return err
		}
		for i := range n {
			c := &d.components[indices[i]]
			h, v := int(c.h), int(c.v)
			if n == 1 {
				h, v = 1, 1
			}
			for b := range h * v {
				x, y := mcu%mw*h+b%h, mcu/mw*v+b/h
				inside := x < c.w && y < c.hBlocks
				var previous int16
				if inside {
					previous = c.dc[y*c.w+x]
				}
				dc, err := d.blockDC(&d.huffman[0][p[2+2*i]>>4], &predictors[i], ah, al, previous)
				if err != nil {
					return err
				}
				if inside {
					c.dc[y*c.w+x] = dc
				}
				if !d.progressive {
					if err := d.discardAC(&d.huffman[1][p[2+2*i]&15]); err != nil {
						return err
					}
				}
			}
		}
	}
	return d.align()
}

// blockDC decodes one block's DC coefficient. A first scan adds a Huffman-coded
// difference to the predictor; a refinement scan adds one bit to previous.
func (d *visualJPEGDCDecoder) blockDC(
	table *visualJPEGDCHuffman, predictor *int32, ah, al byte, previous int16,
) (int16, error) {
	var dc int32
	if ah != 0 {
		bit, err := d.readBits(1)
		if err != nil {
			return 0, err
		}
		dc = int32(previous) | bit<<al
	} else {
		category, err := d.symbol(table)
		if err != nil {
			return 0, err
		}
		if category > 11 {
			return 0, jpeg.FormatError("bad DC magnitude")
		}
		diff, err := d.readBits(int(category))
		if err != nil {
			return 0, err
		}
		if category > 0 && diff < 1<<(category-1) {
			diff -= 1<<category - 1
		}
		*predictor += diff
		dc = *predictor << al
	}
	if dc < -32768 || dc > 32767 {
		return 0, jpeg.FormatError("DC coefficient overflow")
	}
	return int16(dc), nil
}

func (d *visualJPEGDCDecoder) discardAC(table *visualJPEGDCHuffman) error {
	for k := 1; k < 64; {
		if err := d.check(); err != nil {
			return err
		}
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
	for i := range d.components {
		c := &d.components[i]
		if d.progressive {
			c.quantValue = d.quant[c.quant]
		}
		if c.precision < 0 || c.quantValue == 0 {
			return nil, jpeg.FormatError("missing DC scan or quantization table")
		}
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	bounds := image.Rect(0, 0, (d.width+7)/8, (d.height+7)/8)
	if len(d.components) == 1 {
		gray := image.NewGray(bounds)
		for i := range gray.Pix {
			if err := d.check(); err != nil {
				return nil, err
			}
			gray.Pix[i] = d.components[0].sample(i)
		}
		return gray, nil
	}
	output := image.NewRGBA(bounds)
	rgb := d.isRGB()
	for y := range bounds.Dy() {
		for x := range bounds.Dx() {
			if err := d.check(); err != nil {
				return nil, err
			}
			var values [3]byte
			for i, c := range d.components {
				if !rgb && i > 0 {
					values[i] = c.chroma(x, y, d.maxH, d.maxV)
				} else {
					cx, cy := x*int(c.h)/d.maxH, y*int(c.v)/d.maxV
					values[i] = c.sample(cy*c.w + cx)
				}
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

func (c *visualJPEGDCComponent) chroma(x, y, maxH, maxV int) byte {
	// Align chroma block centres with output pixel centres before interpolating.
	sx, sy := 2*maxH, 2*maxV
	fx := max(0, min((c.w-1)*sx, (2*x+1)*int(c.h)-maxH))
	fy := max(0, min((c.hBlocks-1)*sy, (2*y+1)*int(c.v)-maxV))
	x0, y0 := fx/sx, fy/sy
	x1, y1 := min(x0+1, c.w-1), min(y0+1, c.hBlocks-1)
	dx, dy := fx%sx, fy%sy
	top := int(c.sample(y0*c.w+x0))*(sx-dx) + int(c.sample(y0*c.w+x1))*dx
	bottom := int(c.sample(y1*c.w+x0))*(sx-dx) + int(c.sample(y1*c.w+x1))*dx
	value := (top*(sy-dy) + bottom*dy + sx*sy/2) / (sx * sy)
	return byte(max(0, min(255, value)))
}
