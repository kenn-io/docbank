package media

// AV1 follows the AOM bitstream and ISO BMFF specifications. Sequence headers
// are parsed completely. Frame headers are checked through their dimension and
// reference prefix; entropy/tile decoding is deliberately outside inspection.
const maxMP4AV1OBUs = 1024
const maxMP4AV1ConfigurationBytes = 64 * 1024

type mp4AV1Sequence struct {
	dimensions                  mp4SampleDimensions
	widthBits, heightBits       int
	reduced, still, superres    bool
	orderBits                   int
	screenTools, integerMV      uint64
	profile, level, tier, color uint64
}

type mp4AV1Reference struct {
	dimensions           mp4SampleDimensions
	orderHint, frameType uint64
	showable             bool
}

type mp4AV1State struct {
	pictureBudget int64
	configuration [3]byte
	sequence      mp4AV1Sequence
	references    [8]mp4AV1Reference
}

func newMP4AV1State(config []byte) (*mp4AV1State, bool) {
	if len(config) < 4 || len(config) > maxMP4AV1ConfigurationBytes || config[0] != 0x81 || config[3]&0xe0 != 0 || config[3]&0x10 == 0 && config[3]&15 != 0 {
		return nil, false
	}
	profile, level := config[1]>>5, config[1]&31
	color := config[2]
	if profile > 2 || !validAV1Level(uint64(level)) || level <= 7 && color&0x80 != 0 || color&0x20 != 0 && (profile != 2 || color&0x40 == 0) || color&3 == 3 {
		return nil, false
	}
	s := &mp4AV1State{configuration: [3]byte{config[1], config[2], config[3]}}
	remaining := config[4:]
	// The bounded supported configOBUs form is zero or one sequence header.
	// Metadata/configuration types that need separate interpretation fail closed.
	if len(remaining) > 0 {
		kind, payload, rest, ok := av1NextOBU(remaining)
		if !ok || kind != 1 || len(rest) != 0 || remaining[0]&2 == 0 || !s.setSequence(payload) {
			return nil, false
		}
	}
	s.pictureBudget = maxMP4InspectedPictures
	return s, true
}

func validAV1Level(level uint64) bool { return level <= 23 || level == 31 }

func av1NextOBU(data []byte) (byte, []byte, []byte, bool) {
	if len(data) == 0 || data[0]&0x81 != 0 {
		return 0, nil, nil, false
	}
	header := data[0]
	kind := header >> 3 & 15
	data = data[1:]
	if header&4 != 0 {
		// Single-layer subset: all temporal/spatial IDs and reserved bits are zero.
		if len(data) == 0 || data[0] != 0 {
			return 0, nil, nil, false
		}
		data = data[1:]
	}
	if header&2 == 0 {
		return kind, data, nil, true
	} // final OBU consumes sample remainder
	var size uint64
	for i := range 8 {
		if len(data) == 0 {
			return 0, nil, nil, false
		}
		value := data[0]
		data = data[1:]
		size |= uint64(value&127) << (7 * i)
		if value&128 == 0 {
			// AV1 allows up to eight LEB128 bytes, but the value fits uint32.
			if size > 0xffffffff || size > uint64(len(data)) {
				return 0, nil, nil, false
			}
			return kind, data[:int(size)], data[int(size):], true
		}
	}
	return 0, nil, nil, false
}

func (state *mp4AV1State) inspectSample(sample []byte) (mp4SampleDimensions, bool) {
	next := *state
	dimensions := next.sequence.dimensions
	picture := false
	for count := 0; len(sample) > 0; count++ {
		if count >= maxMP4AV1OBUs {
			return mp4SampleDimensions{}, false
		}
		kind, payload, rest, ok := av1NextOBU(sample)
		if !ok {
			return mp4SampleDimensions{}, false
		}
		sample = rest
		switch kind {
		case 1:
			if !next.setSequence(payload) {
				return mp4SampleDimensions{}, false
			}
			dimensions.width = max(dimensions.width, next.sequence.dimensions.width)
			dimensions.height = max(dimensions.height, next.sequence.dimensions.height)
		case 2: // temporal delimiter has no payload
			if len(payload) != 0 {
				return mp4SampleDimensions{}, false
			}
		case 3, 6: // show-existing header, or complete frame OBU
			if next.sequence.dimensions.width == 0 || dimensions.frames >= state.pictureBudget {
				return mp4SampleDimensions{}, false
			}
			d, existing, ok := next.inspectFrame(payload)
			if !ok || existing != (kind == 3) {
				return mp4SampleDimensions{}, false
			}
			dimensions.width = max(dimensions.width, d.width)
			dimensions.height = max(dimensions.height, d.height)
			picture = true
			dimensions.frames++
		default:
			// Separate tile groups need final-tile proof outside this subset.
			// Reserved OBUs, metadata, redundant headers, tile lists and padding
			// also fail closed. Never skip an unknown configuration/frame.
			return mp4SampleDimensions{}, false
		}
	}
	if !picture {
		return mp4SampleDimensions{}, false
	}
	*state = next
	return dimensions, true
}

func (state *mp4AV1State) setSequence(payload []byte) bool {
	sequence, ok := av1ReadSequence(payload)
	if !ok || sequence.profile<<5|sequence.level != uint64(state.configuration[0]) || sequence.tier<<7|sequence.color != uint64(state.configuration[1]) {
		return false
	}
	if sequence != state.sequence {
		clear(state.references[:])
	}
	state.sequence = sequence
	return true
}

func av1ReadSequence(payload []byte) (mp4AV1Sequence, bool) {
	var s mp4AV1Sequence
	if len(payload) == 0 || len(payload) > maxMP4AV1ConfigurationBytes {
		return s, false
	}
	r := mp4HeaderReader{data: payload}
	s.profile = r.read(3)
	s.still, s.reduced = r.read(1) != 0, r.read(1) != 0
	if s.profile > 2 || s.reduced && !s.still {
		return s, false
	}
	if s.reduced {
		s.level = r.read(5)
	} else {
		// Timing/decoder models and multiple operating points require additional
		// frame-prefix state; reject them instead of skipping uncertain fields.
		if r.read(1) != 0 {
			return s, false
		}
		initialDelay := r.read(1) != 0
		if r.read(5) != 0 || r.read(12) != 0 {
			return s, false
		}
		s.level = r.read(5)
		if s.level > 7 {
			s.tier = r.read(1)
		}
		if initialDelay && r.read(1) != 0 {
			r.skip(4)
		}
	}
	if !validAV1Level(s.level) {
		return s, false
	}
	s.widthBits, s.heightBits = int(r.read(4)&15)+1, int(r.read(4)&15)+1
	s.dimensions = mp4SampleDimensions{width: int64(r.read(s.widthBits)) + 1, height: int64(r.read(s.heightBits)) + 1} //nolint:gosec // dimension fields are at most 16 bits
	if !s.reduced && r.read(1) != 0 {
		return s, false
	} // frame IDs unsupported
	r.skip(3) // superblock, filter intra, intra edge
	s.screenTools, s.integerMV = 2, 2
	if !s.reduced {
		r.skip(4) // interintra, masked compound, warped motion, dual filter
		orderHint := r.read(1) != 0
		if orderHint {
			r.skip(2)
		}
		if r.read(1) == 0 {
			s.screenTools = r.read(1)
		}
		if s.screenTools > 0 && r.read(1) == 0 {
			s.integerMV = r.read(1)
		}
		if orderHint {
			s.orderBits = int(r.read(3)&7) + 1
		}
	}
	s.superres = r.read(1) != 0
	r.skip(2) // cdef, restoration
	color, ok := av1ReadColor(&r, s.profile)
	if !ok {
		return s, false
	}
	s.color = color
	r.skip(1) // film grain present; does not alter encoded frame dimensions
	if r.read(1) != 1 {
		return s, false
	} // trailing_one_bit
	padding := len(payload)*8 - r.bit
	if padding < 0 || padding > 7 || r.read(padding) != 0 || r.failed {
		return s, false
	}
	return s, true
}

func av1ReadColor(r *mp4HeaderReader, profile uint64) (uint64, bool) {
	high, twelve := r.read(1), uint64(0)
	if profile == 2 && high != 0 {
		twelve = r.read(1)
	}
	mono := uint64(0)
	if profile != 1 {
		mono = r.read(1)
	}
	primaries, transfer, matrix := uint64(2), uint64(2), uint64(2)
	if r.read(1) != 0 {
		primaries, transfer, matrix = r.read(8), r.read(8), r.read(8)
	}
	if !validMP4ColorCodes(primaries, transfer, matrix) {
		return 0, false
	}
	x, y, position := uint64(0), uint64(0), uint64(0)
	if mono != 0 {
		r.skip(1)
		x, y = 1, 1
	} else {
		if primaries == 1 && transfer == 13 && matrix == 0 {
			if profile != 1 && (profile != 2 || twelve == 0) {
				return 0, false
			}
		} else {
			r.skip(1)
			switch profile {
			case 0:
				x, y = 1, 1
			case 1:
			case 2:
				if twelve != 0 {
					x = r.read(1)
					if x != 0 {
						y = r.read(1)
					}
				} else {
					x = 1
				}
			}
			if x != 0 && y != 0 {
				position = r.read(2)
				if position == 3 {
					return 0, false
				}
			}
			if matrix == 0 && (x != 0 || y != 0) {
				return 0, false
			}
		}
		r.skip(1) // separate_uv_delta_q
	}
	return high<<6 | twelve<<5 | mono<<4 | x<<3 | y<<2 | position, !r.failed
}

func (state *mp4AV1State) inspectFrame(payload []byte) (mp4SampleDimensions, bool, bool) {
	seq := state.sequence
	r := mp4HeaderReader{data: payload}
	frameType, show, resilient := uint64(0), true, true
	showable := false // Reduced-still and shown KEY frames cannot be shown again.
	if !seq.reduced {
		if r.read(1) != 0 {
			ref := state.references[r.read(3)]
			if r.failed || ref.dimensions.width == 0 || !ref.showable {
				return mp4SampleDimensions{}, false, false
			}
			// show_existing_frame terminates a frame-header OBU with trailing bits.
			if r.read(1) != 1 {
				return mp4SampleDimensions{}, false, false
			}
			padding := len(payload)*8 - r.bit
			if padding > 7 || padding < 0 || r.read(padding) != 0 || r.failed {
				return mp4SampleDimensions{}, false, false
			}
			if ref.frameType == 0 {
				// Showing a hidden KEY refreshes every slot. Mark the shared
				// picture consumed before copying, preventing reuse via any alias.
				ref.showable = false
				for i := range state.references {
					state.references[i] = ref
				}
			}
			return ref.dimensions, true, true
		}
		frameType, show = r.read(2), r.read(1) != 0
		if show {
			showable = frameType != 0
		} else {
			showable = r.read(1) != 0
		}
		if frameType != 3 && (frameType != 0 || !show) {
			resilient = r.read(1) != 0
		}
	}
	if seq.still && (frameType != 0 || !show) {
		return mp4SampleDimensions{}, false, false
	}
	intra := frameType == 0 || frameType == 2
	if frameType == 0 && show {
		clear(state.references[:])
	}
	r.skip(1) // disable_cdf_update
	screen := seq.screenTools
	if screen == 2 {
		screen = r.read(1)
	}
	if screen != 0 && seq.integerMV == 2 {
		r.skip(1)
	}
	override := frameType == 3
	if !seq.reduced && frameType != 3 {
		override = r.read(1) != 0
	}
	hint := r.read(seq.orderBits)
	if !intra && !resilient {
		r.skip(3)
	} // primary_ref_frame
	refresh := uint64(255)
	if frameType != 3 && (frameType != 0 || !show) {
		refresh = r.read(8)
	}
	if !intra || refresh != 255 {
		if resilient && seq.orderBits > 0 {
			for i := range state.references {
				if r.read(seq.orderBits) != state.references[i].orderHint {
					state.references[i] = mp4AV1Reference{}
				}
			}
		}
	}
	var d mp4SampleDimensions
	if !intra {
		if seq.orderBits > 0 && r.read(1) != 0 {
			return d, false, false
		} // short ref signaling unsupported
		var refs [7]mp4AV1Reference
		for i := range refs {
			refs[i] = state.references[r.read(3)]
			if refs[i].dimensions.width == 0 {
				return d, false, false
			}
		}
		if override && !resilient {
			for _, ref := range refs {
				if r.read(1) != 0 {
					d = ref.dimensions
					break
				}
			}
		}
	}
	inherited := d.width != 0
	if !inherited {
		d = seq.dimensions
		if override {
			d = mp4SampleDimensions{width: int64(r.read(seq.widthBits)) + 1, height: int64(r.read(seq.heightBits)) + 1} //nolint:gosec // sequence dimension fields are at most 16 bits
		}
	}
	if d.width > seq.dimensions.width || d.height > seq.dimensions.height {
		return d, false, false
	}
	// Super-resolution only reduces coded width; retain the upscaled bound.
	usesSuperres := seq.superres && r.read(1) != 0
	if usesSuperres {
		r.skip(3)
	}
	rendered := d
	if !inherited && r.read(1) != 0 {
		rendered = mp4SampleDimensions{width: int64(r.read(16)) + 1, height: int64(r.read(16)) + 1} //nolint:gosec // 16-bit fields
	}
	if intra && screen != 0 && !usesSuperres {
		r.skip(1)
	} // allow_intrabc (conservative prefix subset)
	// Require content beyond the dimension-bearing prefix. This is structural
	// header authority, not a claim that opaque entropy bytes decode correctly.
	if r.failed || (r.bit+7)/8 >= len(payload) {
		return mp4SampleDimensions{}, false, false
	}
	for i := range state.references {
		if refresh>>i&1 != 0 {
			state.references[i] = mp4AV1Reference{d, hint, frameType, showable}
		}
	}
	return mp4SampleDimensions{width: max(d.width, rendered.width), height: max(d.height, rendered.height)}, false, true
}
