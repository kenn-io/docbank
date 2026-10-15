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
func M4A(codec string) []byte {
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
	return slices.Concat(ftyp, mdat, Box("moov", slices.Concat(Box("mvhd", mvhd), Box("trak", slices.Concat(Box("tkhd", tkhd), mdia)))))
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
