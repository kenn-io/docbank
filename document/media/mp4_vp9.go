package media

// VP9 syntax follows libvpx's read_uncompressed_header and the WebM VP codec
// ISO media binding. We inspect headers and partition lengths, not entropy data.
type mp4VP9State struct {
	pictureBudget                                                  int64
	profile, depth, chroma, fullRange, primaries, transfer, matrix uint64
	references                                                     [8]mp4SampleDimensions
}

func newMP4VP9State(config []byte) (*mp4VP9State, bool) {
	// vpcC version 1, zero flags, and no codec initialization data for VP9.
	if len(config) != 12 || config[0] != 1 || config[1] != 0 || config[2] != 0 || config[3] != 0 || config[10] != 0 || config[11] != 0 {
		return nil, false
	}
	s := &mp4VP9State{profile: uint64(config[4]), depth: uint64(config[6] >> 4), chroma: uint64(config[6] >> 1 & 7), fullRange: uint64(config[6] & 1), primaries: uint64(config[7]), transfer: uint64(config[8]), matrix: uint64(config[9])}
	switch config[5] {
	case 0, 10, 11, 20, 21, 30, 31, 40, 41, 50, 51, 52, 60, 61, 62:
	default:
		return nil, false
	}
	if s.profile > 3 || s.chroma > 3 || s.profile < 2 && s.depth != 8 || s.profile >= 2 && s.depth != 10 && s.depth != 12 || s.profile%2 == 0 && s.chroma > 1 || s.profile%2 == 1 && s.chroma < 2 || s.matrix == 0 && s.chroma != 3 {
		return nil, false
	}
	if !validMP4ColorCodes(s.primaries, s.transfer, s.matrix) {
		return nil, false
	}
	s.pictureBudget = maxMP4InspectedPictures
	return s, true
}

func (state *mp4VP9State) inspectSample(sample []byte) (mp4SampleDimensions, bool) {
	if len(sample) == 0 {
		return mp4SampleDimensions{}, false
	}
	next := *state
	var dimensions mp4SampleDimensions
	frameCount := 1
	var sizes [8]int
	marker := sample[len(sample)-1]
	if marker&0xe0 == 0xc0 {
		frameCount = int(marker&7) + 1
		magnitude := int(marker>>3&3) + 1
		indexBytes := 2 + frameCount*magnitude
		if indexBytes > len(sample) || sample[len(sample)-indexBytes] != marker {
			return dimensions, false
		}
		cursor := len(sample) - indexBytes + 1
		total := uint64(0)
		for i := range frameCount {
			size := uint64(0)
			for j := range magnitude {
				size |= uint64(sample[cursor]) << (8 * j)
				cursor++
			}
			if size == 0 || size > uint64(len(sample)-indexBytes)-total { //nolint:gosec // indexBytes <= len(sample), total <= remaining payload
				return dimensions, false
			}
			sizes[i] = int(size) //nolint:gosec // three-bit count plus one <= len(sizes); size <= payload length
			total += size
		}
		if total != uint64(len(sample)-indexBytes) { //nolint:gosec // indexBytes <= len(sample)
			return dimensions, false
		}
	} else {
		sizes[0] = len(sample)
	}
	cursor := 0
	if int64(frameCount) > state.pictureBudget {
		return mp4SampleDimensions{}, false
	}
	for i := range frameCount {
		d, ok := next.inspectFrame(sample[cursor : cursor+sizes[i]])
		if !ok {
			return mp4SampleDimensions{}, false
		}
		dimensions.width = max(dimensions.width, d.width)
		dimensions.height = max(dimensions.height, d.height)
		cursor += sizes[i]
	}
	*state = next
	dimensions.frames = int64(frameCount)
	return dimensions, true
}

func (state *mp4VP9State) inspectFrame(frame []byte) (mp4SampleDimensions, bool) {
	r := mp4HeaderReader{data: frame}
	marker := r.read(2)
	profile := r.read(1) | r.read(1)<<1
	if marker != 2 || profile != state.profile || profile == 3 && r.read(1) != 0 {
		return mp4SampleDimensions{}, false
	}
	if r.read(1) != 0 {
		d := state.references[r.read(3)]
		// show_existing_frame is exactly one (profiles 0..2) or two bytes.
		padding := (8 - r.bit%8) % 8
		if r.read(padding) != 0 || r.failed || r.bit != len(frame)*8 {
			return mp4SampleDimensions{}, false
		}
		return d, d.width > 0 && d.height > 0
	}
	key, show, resilient := r.read(1) == 0, r.read(1) != 0, r.read(1) != 0
	intra := false
	refresh := uint64(255)
	var d mp4SampleDimensions
	if key {
		if r.read(24) != 0x498342 || !state.readColor(&r) {
			return d, false
		}
		d = vp9ReadSize(&r)
	} else {
		if !show {
			intra = r.read(1) != 0
		}
		if !resilient {
			r.skip(2)
		}
		if intra {
			if r.read(24) != 0x498342 {
				return d, false
			}
			if profile > 0 {
				if !state.readColor(&r) {
					return d, false
				}
			} else if !state.colorMatches(8, 1, 0, 1, 1) {
				return d, false
			}
			refresh = r.read(8)
			d = vp9ReadSize(&r)
		} else {
			refresh = r.read(8)
			var refs [3]mp4SampleDimensions
			for i := range refs {
				refs[i] = state.references[r.read(3)]
				r.skip(1)
				if refs[i].width == 0 {
					return d, false
				}
			}
			for _, ref := range refs {
				if r.read(1) != 0 {
					d = ref
					break
				}
			}
			if d.width == 0 {
				d = vp9ReadSize(&r)
			}
			validScale := false
			for _, ref := range refs {
				validScale = validScale || ref.width <= d.width*2 && ref.height <= d.height*2 && d.width <= ref.width*16 && d.height <= ref.height*16
			}
			if !validScale {
				return d, false
			}
		}
	}
	rendered := d
	if r.read(1) != 0 {
		rendered = vp9ReadSize(&r)
	}
	if !key && !intra {
		r.skip(1)
		if r.read(1) == 0 {
			r.skip(2)
		}
	}
	if !resilient {
		r.skip(2)
	}
	r.skip(2) // frame_context_idx
	if !vp9HeaderTail(&r, d.width) {
		return mp4SampleDimensions{}, false
	}
	if key {
		clear(state.references[:])
	}
	for i := range state.references {
		if refresh>>i&1 != 0 {
			state.references[i] = d
		}
	}
	return mp4SampleDimensions{width: max(d.width, rendered.width), height: max(d.height, rendered.height)}, true
}

func vp9ReadSize(r *mp4HeaderReader) mp4SampleDimensions {
	return mp4SampleDimensions{width: int64(r.read(16)) + 1, height: int64(r.read(16)) + 1} //nolint:gosec // 16-bit fields
}

func (state *mp4VP9State) readColor(r *mp4HeaderReader) bool {
	depth := uint64(8)
	if state.profile >= 2 {
		depth = 10 + 2*r.read(1)
	}
	space := r.read(3)
	if space == 6 {
		return false
	} // reserved
	full, x, y := uint64(1), uint64(0), uint64(0)
	if space != 7 {
		full = r.read(1)
		x, y = 1, 1
		if state.profile%2 != 0 {
			x, y = r.read(1), r.read(1)
			if r.read(1) != 0 || x == 1 && y == 1 {
				return false
			}
		}
	} else if state.profile%2 == 0 || r.read(1) != 0 {
		return false
	}
	return !r.failed && state.colorMatches(depth, space, full, x, y)
}

func (state *mp4VP9State) colorMatches(depth, space, full, x, y uint64) bool {
	chroma := uint64(3)
	if x == 1 {
		if y == 1 {
			chroma = state.chroma
			if chroma > 1 {
				return false
			}
		} else {
			chroma = 2
		}
	} else if y != 0 {
		return false
	}
	if state.depth != depth || state.chroma != chroma || state.fullRange != full {
		return false
	}
	// Unspecified container color metadata can describe any codec color space.
	// VP9 UNKNOWN leaves these fields unspecified; explicit spaces must agree.
	if space == 0 {
		return true
	}
	matrix, primaries, transfer := uint64(2), uint64(2), uint64(2)
	switch space {
	case 1:
		matrix, primaries, transfer = 6, 6, 6
	case 2:
		matrix, primaries, transfer = 1, 1, 1
	case 3:
		matrix, primaries, transfer = 6, 5, 6
	case 4:
		matrix, primaries, transfer = 7, 7, 7
	case 5:
		matrix, primaries, transfer = 9, 9, 14
		if depth == 12 {
			transfer = 15
		}
	case 7:
		matrix, primaries, transfer = 0, 1, 13
	}
	return (state.matrix == 2 || state.matrix == matrix) && (state.primaries == 2 || state.primaries == primaries) && (state.transfer == 2 || state.transfer == transfer)
}

func vp9HeaderTail(r *mp4HeaderReader, width int64) bool {
	r.skip(9) // loop filter level and sharpness
	deltasEnabled := r.read(1) != 0
	if deltasEnabled && r.read(1) != 0 {
		for range 6 {
			if r.read(1) != 0 {
				r.skip(7)
			}
		}
	}
	r.skip(8) // base_q_idx
	for range 3 {
		if r.read(1) != 0 {
			r.skip(5)
		}
	}
	if r.read(1) != 0 { // segmentation_enabled
		if r.read(1) != 0 {
			for range 7 {
				if r.read(1) != 0 {
					r.skip(8)
				}
			}
			if r.read(1) != 0 {
				for range 3 {
					if r.read(1) != 0 {
						r.skip(8)
					}
				}
			}
		}
		if r.read(1) != 0 {
			r.skip(1)
			for range 8 {
				for _, bits := range []int{9, 7, 2, 0} {
					if r.read(1) != 0 {
						r.skip(bits)
					}
				}
			}
		}
	}
	sbCols := (width + 63) / 64
	minimum, maximum := 0, 0
	for (int64(64) << minimum) < sbCols {
		minimum++
	}
	for (sbCols >> maximum) >= 4 {
		maximum++
	}
	maximum = max(0, maximum-1)
	for i := minimum; i < maximum; i++ {
		if r.read(1) == 0 {
			break
		}
	}
	if r.read(1) != 0 {
		r.skip(1)
	}
	partition := r.read(16)
	headerBytes := (r.bit + 7) / 8
	return !r.failed && partition > 0 && headerBytes < len(r.data) && partition < uint64(len(r.data)-headerBytes) //nolint:gosec // headerBytes < len(data)
}
