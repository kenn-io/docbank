// Package mediatest builds small deterministic synthetic media containers for
// tests. The WebP and MP4 builders produce minimal headers that satisfy
// signature and metadata detection; they are not decodable pictures and must
// not be used as provider probe fixtures.
package mediatest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"slices"
)

// JPEG returns a solid JPEG of the given dimensions.
func JPEG(width, height int, fill color.Color) []byte {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, solid(width, height, fill), &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// PNG returns a solid PNG of the given dimensions.
func PNG(width, height int, fill color.Color) []byte {
	var out bytes.Buffer
	if err := png.Encode(&out, solid(width, height, fill)); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// GIF returns a GIF with the given number of frames. Frames alternate between
// two palette colors so an animated result differs from its first frame.
func GIF(width, height, frames int) []byte {
	palette := color.Palette{color.Black, color.White}
	animation := &gif.GIF{}
	for frame := range frames {
		img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		if frame%2 == 1 {
			for index := range img.Pix {
				img.Pix[index] = 1
			}
		}
		animation.Image = append(animation.Image, img)
		animation.Delay = append(animation.Delay, 10)
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, animation); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// WebP returns a minimal VP8X WebP header declaring the given canvas size.
func WebP(width, height int) []byte {
	data := make([]byte, 44)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8)) //nolint:gosec // synthetic fixture is small
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	w := width - 1
	h := height - 1
	data[24], data[25], data[26] = byte(w), byte(w>>8), byte(w>>16) //nolint:gosec // synthetic canvas sizes are small
	data[27], data[28], data[29] = byte(h), byte(h>>8), byte(h>>16) //nolint:gosec // synthetic canvas sizes are small
	copy(data[30:34], "VP8L")
	binary.LittleEndian.PutUint32(data[34:38], 5)
	data[38] = 0x2f
	binary.LittleEndian.PutUint32(data[39:43], uint32(width-1)|uint32(height-1)<<14) //nolint:gosec // synthetic dimensions are small
	return data
}

// MP4 returns a minimal ISO base media file with movie, track, and visual
// sample-description boxes declaring the given dimensions and duration.
func MP4(width, height int, durationMS int64) []byte {
	ftypPayload := append([]byte("isom"), make([]byte, 12)...)
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS)) //nolint:gosec // synthetic fixture durations are small
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-8:len(tkhd)-4], uint32(width<<16)) //nolint:gosec // synthetic fixture dimensions are small
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-4:], uint32(height<<16))           //nolint:gosec // synthetic fixture dimensions are small
	trak := Box("trak", append(Box("tkhd", tkhd), visualSampleTable(width, height, durationMS)...))
	moov := Box("moov", append(Box("mvhd", mvhd), trak...))
	return append(Box("ftyp", ftypPayload), moov...)
}

func visualSampleTable(width, height int, durationMS int64) []byte {
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], uint16(width))  //nolint:gosec // synthetic dimensions are small
	binary.BigEndian.PutUint16(entry[26:28], uint16(height)) //nolint:gosec // synthetic dimensions are small
	entry = slices.Concat(entry, Box("avcC", AVCConfig(width, height)))
	stsd := make([]byte, 0, 8+8+len(entry))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, Box("avc1", entry)...)
	handler := make([]byte, 12)
	copy(handler[8:12], "vide")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(durationMS)) //nolint:gosec // synthetic fixture durations are small
	stts := make([]byte, 8)
	if durationMS > 0 {
		binary.BigEndian.PutUint32(stts[4:8], 1)
		entry := make([]byte, 8)
		binary.BigEndian.PutUint32(entry[:4], 1)
		binary.BigEndian.PutUint32(entry[4:8], uint32(durationMS)) //nolint:gosec // synthetic fixture durations are small
		stts = slices.Concat(stts, entry)
	}
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], 1)
	if durationMS > 0 {
		binary.BigEndian.PutUint32(stsz[8:12], 1)
	}
	stbl := Box("stbl", append(append(Box("stsd", stsd), Box("stts", stts)...), Box("stsz", stsz)...))
	return Box("mdia", append(append(Box("mdhd", mdhd), Box("hdlr", handler)...), Box("minf", stbl)...))
}

// AVCConfig returns a minimal AVCDecoderConfigurationRecord whose sequence
// parameter set declares the requested even dimensions.
func AVCConfig(width, height int) []byte {
	sps := avcSPS(width, height)
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	config := []byte{1, 66, 0, 40, 0xff, 0xe1}
	config = binary.BigEndian.AppendUint16(config, uint16(len(sps))) //nolint:gosec // synthetic SPS is bounded
	config = append(config, sps...)
	config = append(config, 1)
	config = binary.BigEndian.AppendUint16(config, uint16(len(pps))) //nolint:gosec // fixed synthetic PPS is four bytes
	return append(config, pps...)
}

// HEVCConfig returns a minimal HEVCDecoderConfigurationRecord whose sequence
// parameter set declares coded dimensions and an optional smaller visible
// window. All dimensions must be positive and even.
func HEVCConfig(codedWidth, codedHeight, visibleWidth, visibleHeight int) []byte {
	if codedWidth < visibleWidth || codedHeight < visibleHeight || visibleWidth < 2 || visibleHeight < 2 ||
		codedWidth > math.MaxUint16 || codedHeight > math.MaxUint16 ||
		codedWidth%2 != 0 || codedHeight%2 != 0 || visibleWidth%2 != 0 || visibleHeight%2 != 0 {
		return nil
	}
	writer := bitWriter{}
	writer.bits(0, 4) // sps_video_parameter_set_id
	writer.bits(0, 3) // sps_max_sub_layers_minus1
	writer.bits(1, 1) // sps_temporal_id_nesting_flag
	writer.bits(1, 8) // general profile fields
	writer.bits(0, 32)
	writer.bits(0, 48)
	writer.bits(120, 8)            // general_level_idc
	writer.ue(0)                   // sps_seq_parameter_set_id
	writer.ue(1)                   // chroma_format_idc: 4:2:0
	writer.ue(uint64(codedWidth))  //nolint:gosec // validated positive and at most MaxUint16
	writer.ue(uint64(codedHeight)) //nolint:gosec // validated positive and at most MaxUint16
	if codedWidth != visibleWidth || codedHeight != visibleHeight {
		writer.bits(1, 1) // conformance_window_flag
		writer.ue(0)
		writer.ue(uint64((codedWidth - visibleWidth) / 2)) //nolint:gosec // validated non-negative
		writer.ue(0)
		writer.ue(uint64((codedHeight - visibleHeight) / 2)) //nolint:gosec // validated non-negative
	} else {
		writer.bits(0, 1)
	}
	writer.bits(1, 1)
	writer.align()
	sps := append([]byte{0x42, 0x01}, writer.data...)

	config := make([]byte, 23+3+2+len(sps))
	config[0] = 1
	config[22] = 1
	config[23] = 0xa1 // complete SPS array
	binary.BigEndian.PutUint16(config[24:26], 1)
	binary.BigEndian.PutUint16(config[26:28], uint16(len(sps))) //nolint:gosec // synthetic SPS is bounded
	copy(config[28:], sps)
	return config
}

func avcSPS(width, height int) []byte {
	if width < 2 || height < 2 || width > math.MaxUint16 || height > math.MaxUint16 {
		return nil
	}
	codedWidth := (width + 15) / 16 * 16
	codedHeight := (height + 15) / 16 * 16
	writer := bitWriter{}
	writer.bits(66, 8) // baseline profile
	writer.bits(0, 8)
	writer.bits(40, 8)
	writer.ue(0) // seq_parameter_set_id
	writer.ue(0) // log2_max_frame_num_minus4
	writer.ue(0) // pic_order_cnt_type
	writer.ue(0) // log2_max_pic_order_cnt_lsb_minus4
	writer.ue(1) // max_num_ref_frames
	writer.bits(0, 1)
	writer.ue(uint64(codedWidth/16 - 1))
	writer.ue(uint64(codedHeight/16 - 1))
	writer.bits(1, 1) // frame_mbs_only_flag
	writer.bits(1, 1) // direct_8x8_inference_flag
	cropRight, cropBottom := (codedWidth-width)/2, (codedHeight-height)/2
	if cropRight > 0 || cropBottom > 0 {
		writer.bits(1, 1)
		writer.ue(0)
		writer.ue(uint64(cropRight)) //nolint:gosec // non-negative by construction
		writer.ue(0)
		writer.ue(uint64(cropBottom)) //nolint:gosec // non-negative by construction
	} else {
		writer.bits(0, 1)
	}
	writer.bits(0, 1) // vui_parameters_present_flag
	writer.bits(1, 1) // rbsp_stop_one_bit
	writer.align()
	return append([]byte{0x67}, writer.data...)
}

type bitWriter struct {
	data []byte
	bit  int
}

func (w *bitWriter) bits(value uint64, count int) {
	for shift := count - 1; shift >= 0; shift-- {
		if w.bit%8 == 0 {
			w.data = append(w.data, 0)
		}
		w.data[len(w.data)-1] |= byte(value>>shift&1) << (7 - w.bit%8)
		w.bit++
	}
}

func (w *bitWriter) ue(value uint64) {
	code := value + 1
	bits := 0
	for current := code; current > 0; current >>= 1 {
		bits++
	}
	w.bits(0, bits-1)
	w.bits(code, bits)
}

func (w *bitWriter) align() {
	if remainder := w.bit % 8; remainder != 0 {
		w.bits(0, 8-remainder)
	}
}

// Box wraps payload in an ISO base media box of the given four-character kind.
func Box(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box[:4], uint32(len(box))) //nolint:gosec // synthetic fixture boxes are small
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func solid(width, height int, fill color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	if fill == nil {
		fill = color.RGBA{R: 200, G: 40, B: 40, A: 255}
	}
	for y := range height {
		for x := range width {
			img.Set(x, y, fill)
		}
	}
	return img
}
