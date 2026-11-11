package mediatest

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"slices"
)

// M4A builds a synthetic audio-only MP4 with one 1-second sound sample.
// The sample bytes are placeholders, not a provider or decoder probe fixture.
func M4A(codec string) []byte { return m4a(codec, nil) }

// SoundEdit is one edit-list entry: Duration in movie milliseconds and
// MediaTime in 48 kHz media samples, or -1 for an empty edit.
type SoundEdit struct {
	Duration  uint32
	MediaTime int32
}

// M4AWithSoundEdits is M4A with an edit list on its sound track.
func M4AWithSoundEdits(codec string, edits ...SoundEdit) []byte {
	elst := binary.BigEndian.AppendUint32(make([]byte, 4), uint32(len(edits))) //nolint:gosec // tiny fixture
	for _, edit := range edits {
		elst = binary.BigEndian.AppendUint32(elst, edit.Duration)
		elst = binary.BigEndian.AppendUint32(elst, uint32(edit.MediaTime)) //nolint:gosec // -1 encodes an empty edit
		elst = append(elst, 0, 1, 0, 0)
	}
	return m4a(codec, Box("edts", Box("elst", elst)))
}

func m4a(codec string, edts []byte) []byte {
	ftyp := Box("ftyp", []byte("M4A \x00\x00\x00\x00M4A isommp42"))
	mdat := Box("mdat", []byte{1, 2, 3, 4})
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], 1000)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[12:16], 1)
	binary.BigEndian.PutUint32(tkhd[20:24], 1000)
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 48000)
	binary.BigEndian.PutUint32(mdhd[16:20], 48000)
	hdlr := make([]byte, 24)
	copy(hdlr[8:12], "soun")
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[6:8], 1)
	binary.BigEndian.PutUint16(entry[16:18], 1)
	binary.BigEndian.PutUint16(entry[18:20], 16)
	binary.BigEndian.PutUint32(entry[24:28], 48000<<16)
	switch codec {
	case "mp4a":
		// ES descriptor, MPEG-4 AAC decoder config, AudioSpecificConfig AAC-LC.
		esds := []byte{0, 0, 0, 0, 3, 25, 0, 1, 0, 4, 17, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 2, 0x11, 0x88, 6, 1, 2}
		entry = slices.Concat(entry, Box("esds", esds))
	case "alac":
		config := make([]byte, 28)
		binary.BigEndian.PutUint32(config[4:8], 4096)
		config[9], config[13] = 16, 1
		binary.BigEndian.PutUint32(config[24:28], 48000)
		entry = slices.Concat(entry, Box("alac", config))
	}
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, Box(codec, entry)...)
	stts := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0xbb, 0x80}
	stsz := []byte{0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0, 1}
	stsc := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:8], 1)
	binary.BigEndian.PutUint32(stco[8:12], uint32(len(ftyp)+8)) //nolint:gosec // tiny fixture
	dref := Box("dref", append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, Box("url ", []byte{0, 0, 0, 1})...))
	stbl := Box("stbl", slices.Concat(Box("stsd", stsd), Box("stts", stts), Box("stsz", stsz), Box("stsc", stsc), Box("stco", stco)))
	mdia := Box("mdia", slices.Concat(Box("mdhd", mdhd), Box("hdlr", hdlr), Box("minf", slices.Concat(Box("dinf", dref), stbl))))
	return slices.Concat(ftyp, mdat, Box("moov", slices.Concat(Box("mvhd", mvhd), Box("trak", slices.Concat(Box("tkhd", tkhd), edts, mdia)))))
}

// SizedM4A streams an M4A of exactly size bytes whose one AAC sample sits at
// the end of a zero-filled media-data box, so its chunk offset approaches size.
func SizedM4A(size int64) io.Reader {
	data := M4A("mp4a")
	mdat := bytes.Index(data, []byte("mdat")) - 4
	moov := bytes.Index(data, []byte("moov")) - 4
	movie := slices.Clone(data[moov:])
	mdatBytes := size - int64(mdat) - int64(len(movie))
	sampleOffset := int64(mdat) + mdatBytes - 4
	if mdatBytes < 12 || mdatBytes > math.MaxUint32 || sampleOffset > math.MaxUint32 {
		panic("invalid synthetic M4A size")
	}
	header := slices.Clone(data[:mdat+8])
	binary.BigEndian.PutUint32(header[mdat:mdat+4], uint32(mdatBytes))
	stco := bytes.Index(movie, []byte("stco")) + 4
	binary.BigEndian.PutUint32(movie[stco+8:stco+12], uint32(sampleOffset)) //nolint:gosec // checked against MaxUint32 above
	return io.MultiReader(bytes.NewReader(header), io.LimitReader(zeroReader{}, mdatBytes-8), bytes.NewReader(movie))
}

// AACM4A returns 50 ms of synthetic silent AAC in an M4A container.
// Generated with ffmpeg from anullsrc=r=48000:cl=mono, -t 0.05,
// -c:a aac, and -map_metadata -1; no source recording is used.
func AACM4A() []byte {
	return decodeFixture("AAAAHGZ0eXBNNEEgAAACAE00QSBpc29taXNvMgAAAAhmcmVlAAAAJ21kYXTcAExhdmM2My4xLjEwMgACMEAOARggBwEYIAcBGCAHAAADCm1vb3YAAABsbXZoZAAAAAAAAAAAAAAAAAAAu4AAAAlgAAEAAAEAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIAAAI1dHJhawAAAFx0a2hkAAAAAwAAAAAAAAAAAAAAAQAAAAAAAAlgAAAAAAAAAAAAAAABAQAAAAABAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAJGVkdHMAAAAcZWxzdAAAAAAAAAABAAAJYAAABAAAAQAAAAABrW1kaWEAAAAgbWRoZAAAAAAAAAAAAAAAAAAAu4AAAA1gVcQAAAAAAC1oZGxyAAAAAAAAAABzb3VuAAAAAAAAAAAAAAAAU291bmRIYW5kbGVyAAAAAVhtaW5mAAAAEHNtaGQAAAAAAAAAAAAAACRkaW5mAAAAHGRyZWYAAAAAAAAAAQAAAAx1cmwgAAAAAQAAARxzdGJsAAAAanN0c2QAAAAAAAAAAQAAAFptcDRhAAAAAAAAAAEAAAAAAAAAAAABABAAAAAAu4AAAAAAADZlc2RzAAAAAAOAgIAlAAEABICAgBdAFQAAAAABDYgAAA2UBYCAgAURiFblAAaAgIABAgAAACBzdHRzAAAAAAAAAAIAAAADAAAEAAAAAAEAAAFgAAAAHHN0c2MAAAAAAAAAAQAAAAEAAAAEAAAAAQAAACRzdHN6AAAAAAAAAAAAAAAEAAAAEwAAAAQAAAAEAAAABAAAABRzdGNvAAAAAAAAAAEAAAAsAAAAGnNncGQBAAAAcm9sbAAAAAIAAAAB//8AAAAcc2JncAAAAAByb2xsAAAAAQAAAAQAAAABAAAAYXVkdGEAAABZbWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAbWRpcmFwcGwAAAAAAAAAAAAAAAAsaWxzdAAAACSpdG9vAAAAHGRhdGEAAAABAAAAAExhdmY2My4xLjEwMg==")
}

// ALACM4A returns 50 ms of synthetic silent ALAC in an M4A container.
// Generated with ffmpeg from anullsrc=r=48000:cl=mono, -t 0.05,
// -c:a alac, and -map_metadata -1; no source recording is used.
func ALACM4A() []byte {
	return decodeFixture("AAAAHGZ0eXBNNEEgAAACAE00QSBpc29taXNvMgAAAAhmcmVlAAAAH21kYXQAABAAABLAAAAPCAEAAAAAAAAA/4Sv8AAAAqptb292AAAAbG12aGQAAAAAAAAAAAAAAAAAALuAAAAJYAABAAABAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAAAB1XRyYWsAAABcdGtoZAAAAAMAAAAAAAAAAAAAAAEAAAAAAAAJYAAAAAAAAAAAAAAAAQEAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAACRlZHRzAAAAHGVsc3QAAAAAAAAAAQAACWAAAAAAAAEAAAAAAU1tZGlhAAAAIG1kaGQAAAAAAAAAAAAAAAAAALuAAAAJYFXEAAAAAAAtaGRscgAAAAAAAAAAc291bgAAAAAAAAAAAAAAAFNvdW5kSGFuZGxlcgAAAAD4bWluZgAAABBzbWhkAAAAAAAAAAAAAAAkZGluZgAAABxkcmVmAAAAAAAAAAEAAAAMdXJsIAAAAAEAAAC8c3RibAAAAFhzdHNkAAAAAAAAAAEAAABIYWxhYwAAAAAAAAABAAAAAAAAAAAAAQAQAAAAALuAAAAAAAAkYWxhYwAAAAAAABAAABAoCg4BAAAAACAEAAu4AAAAu4AAAAAYc3R0cwAAAAAAAAABAAAAAQAACWAAAAAcc3RzYwAAAAAAAAABAAAAAQAAAAEAAAABAAAAFHN0c3oAAAAAAAAAFwAAAAEAAAAUc3RjbwAAAAAAAAABAAAALAAAAGF1ZHRhAAAAWW1ldGEAAAAAAAAAIWhkbHIAAAAAAAAAAG1kaXJhcHBsAAAAAAAAAAAAAAAALGlsc3QAAAAkqXRvbwAAABxkYXRhAAAAAQAAAABMYXZmNjMuMS4xMDI=")
}

// SizedMP4Video streams a synthetic MP4 with qualified AAC and 10,001 video
// samples. Sample payloads are placeholders, never a provider/decoder fixture.
func SizedMP4Video(size int64) io.Reader { return sizedMP4Video(size, false) }

// SizedMP4VideoComposition is SizedMP4Video with a valid video composition-offset table.
func SizedMP4VideoComposition(size int64) io.Reader { return sizedMP4Video(size, true) }

func sizedMP4Video(size int64, composition bool) io.Reader {
	data := M4A("mp4a")
	mdat := bytes.Index(data, []byte("mdat")) - 4
	moov := bytes.Index(data, []byte("moov")) - 4
	movie := slices.Clone(data[moov:])
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[12:16], 2)
	binary.BigEndian.PutUint32(tkhd[76:80], 32<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], 18<<16)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[6:8], 1)
	binary.BigEndian.PutUint16(entry[24:26], 32)
	binary.BigEndian.PutUint16(entry[26:28], 18)
	entry = slices.Concat(entry, Box("avcC", AVCConfig(32, 18)))
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, Box("avc1", entry)...)
	stts := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0x27, 0x11, 0, 0, 0, 1}
	stsz := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0x27, 0x11}
	stsc := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0x27, 0x11, 0, 0, 0, 1}
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:8], 1)
	binary.BigEndian.PutUint32(stco[8:12], uint32(mdat+8)) //nolint:gosec // bounded fixture header
	dref := Box("dref", append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, Box("url ", []byte{0, 0, 0, 1})...))
	hdlr := make([]byte, 24)
	copy(hdlr[8:12], "vide")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1000)
	binary.BigEndian.PutUint32(mdhd[16:20], 10001)
	boxes := [][]byte{Box("stsd", stsd), Box("stts", stts)}
	if composition {
		boxes = append(boxes, Box("ctts", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0x27, 0x11, 0, 0, 0, 2}))
	}
	boxes = append(boxes, Box("stsz", stsz), Box("stsc", stsc), Box("stco", stco))
	stbl := Box("stbl", slices.Concat(boxes...))
	track := Box("trak", slices.Concat(Box("tkhd", tkhd), Box("mdia", slices.Concat(Box("mdhd", mdhd), Box("hdlr", hdlr), Box("minf", slices.Concat(Box("dinf", dref), stbl))))))
	movie = append(movie, track...)
	binary.BigEndian.PutUint32(movie[:4], uint32(len(movie))) //nolint:gosec // small metadata
	mdatBytes := size - int64(mdat) - int64(len(movie))
	if mdatBytes < 10013 || mdatBytes > math.MaxUint32 {
		panic("invalid synthetic MP4 size")
	}
	header := slices.Clone(data[:mdat+8])
	binary.BigEndian.PutUint32(header[mdat:mdat+4], uint32(mdatBytes))
	audioOffset := bytes.Index(movie, []byte("stco")) + 4
	binary.BigEndian.PutUint32(movie[audioOffset+8:audioOffset+12], uint32(int64(mdat)+mdatBytes-4)) //nolint:gosec // source is at most uint32 bytes
	return io.MultiReader(bytes.NewReader(header), io.LimitReader(zeroReader{}, mdatBytes-8), bytes.NewReader(movie))
}

// H264AACMOV returns a one-second synthetic QuickTime movie with silent AAC.
// Generated with FFmpeg 9.0.2 from color=c=black:s=32x24:r=4 and
// anullsrc=r=22050:cl=mono, using -map_metadata -1 -c:v libx264
// -preset ultrafast -c:a aac -f mov. Its v1 wave/esds layout matches Gate 0.
func H264AACMOV() []byte {
	return decodeFixture("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")
}

// SizedMOV streams the genuine MOV fixture with padding at the end of mdat.
// The original sample offsets and movie metadata are preserved.
func SizedMOV(size int64) io.Reader {
	data := H264AACMOV()
	mdat := bytes.Index(data, []byte("mdat")) - 4
	oldSize := binary.BigEndian.Uint32(data[mdat : mdat+4])
	end := mdat + int(oldSize)
	padding := size - int64(len(data))
	if padding < 0 || padding > math.MaxUint32-int64(oldSize) {
		panic("invalid synthetic MOV size")
	}
	binary.BigEndian.PutUint32(data[mdat:mdat+4], oldSize+uint32(padding)) //nolint:gosec // bounded above
	return io.MultiReader(bytes.NewReader(data[:end]), io.LimitReader(zeroReader{}, padding), bytes.NewReader(data[end:]))
}
