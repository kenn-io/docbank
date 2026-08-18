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
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], 22)
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	w := width - 1
	h := height - 1
	data[24], data[25], data[26] = byte(w), byte(w>>8), byte(w>>16) //nolint:gosec // synthetic canvas sizes are small
	data[27], data[28], data[29] = byte(h), byte(h>>8), byte(h>>16) //nolint:gosec // synthetic canvas sizes are small
	return data
}

// MP4 returns a minimal ISO base media file with ftyp, mvhd, and tkhd boxes
// declaring the given dimensions and duration.
func MP4(width, height int, durationMS int64) []byte {
	ftypPayload := append([]byte("isom"), make([]byte, 12)...)
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS)) //nolint:gosec // synthetic fixture durations are small
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-8:len(tkhd)-4], uint32(width<<16)) //nolint:gosec // synthetic fixture dimensions are small
	binary.BigEndian.PutUint32(tkhd[len(tkhd)-4:], uint32(height<<16))           //nolint:gosec // synthetic fixture dimensions are small
	trak := Box("trak", Box("tkhd", tkhd))
	moov := Box("moov", append(Box("mvhd", mvhd), trak...))
	return append(Box("ftyp", ftypPayload), moov...)
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
