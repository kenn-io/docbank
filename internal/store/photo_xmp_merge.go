package store

import "slices"

type photoXMPComparison struct {
	equal, samePacket, sameCatalog, absent, sticky bool
	choice                                         string
}

// mergePhotoXMPValue selects the catalog (0), sidecar (1), or a conflict (2).
// Only convergence and explicit choices account for a catalog decision.
func mergePhotoXMPValue(input photoXMPComparison) (int, bool) {
	switch {
	case input.equal:
		return 0, true
	case !input.sticky && (input.absent || input.samePacket):
		return 0, false
	case !input.sticky && input.sameCatalog:
		return 1, true
	case input.choice == "catalog":
		return 0, true
	case input.choice == "sidecar":
		return 1, true
	default:
		return 2, false
	}
}

// PhotoXMPHead is one outside source's newest sidecar version when the receipt
// holds an older packet for it. Base is that packet, or the packet of another
// source holding the same bytes, and nil when Docbank has none.
type PhotoXMPHead struct {
	Path, Digest, VersionID string
	Size                    int64
	Base                    *PhotoXMPSnapshot
}

// PhotoXMPStep is one sidecar version compared with the ancestor of its own
// lineage: a source's head, or, with no Head, a version Docbank wrote or committed.
type PhotoXMPStep struct {
	Packet PhotoXMPPacket
	Head   *PhotoXMPHead

	base     *PhotoXMPSnapshot
	baseline PhotoXMPSnapshot
}

// Steps lists the versions to compare, in observation order, ending with the
// current version.
func (w PhotoXMPWork) Steps() []PhotoXMPStep {
	if w.Sidecar == nil {
		return nil
	}
	steps := make([]PhotoXMPStep, 0, len(w.Heads)+1)
	for i := range w.Heads {
		steps = append(steps, w.step(&w.Heads[i]))
	}
	if n := len(w.Heads); n == 0 || w.Heads[n-1].VersionID != w.Sidecar.Node.CurrentVersionID {
		steps = append(steps, w.step(nil))
	}
	return steps
}

func (w PhotoXMPWork) step(h *PhotoXMPHead) PhotoXMPStep {
	s := PhotoXMPStep{Head: h, baseline: PhotoXMPSnapshot{SourceOrientation: w.Orientation}}
	if h != nil {
		s.base = h.Base
	}
	if w.History != nil {
		settled := &w.History.Settled
		if h == nil {
			s.base = settled
		}
		s.baseline = w.baselineFor(h, s.base)
	}
	return s
}

// baselineFor is the snapshot a step's rotation is read against. Bytes Docbank settled
// keep the orientation they were written against, wherever they were copied. A copy with
// no packet of its own is read against the current source, from an empty baseline.
func (w PhotoXMPWork) baselineFor(h *PhotoXMPHead, base *PhotoXMPSnapshot) PhotoXMPSnapshot {
	switch {
	case h == nil || h.Digest == w.History.Settled.Digest:
		return w.History.Settled
	case base != nil:
		return *base
	}
	return PhotoXMPSnapshot{SourceOrientation: w.Orientation}
}

// Baseline is the packet and source orientation the step's rotation is read against.
func (s PhotoXMPStep) Baseline() (PhotoXMPPacket, int) {
	return s.baseline.Packet, s.baseline.SourceOrientation
}

// PlanPhotoXMP compares each step's packet with the last packet of its own
// source, or with Docbank's last packet, on the catalog the step before left.
// The first conflict ends the plan, so a sync packet stays atomic, and a
// conflict or choice binds to the source that caused it. A conflict keeps what
// the steps before it left, so its choice shows and acts on that catalog.
func PlanPhotoXMP(w PhotoXMPWork, steps []PhotoXMPStep, choice string) (PhotoXMPPlan, PhotoAuthoredFields) {
	plan := newPhotoXMPPlan(w, choice)
	var write PhotoAuthoredFields
	for i, s := range steps {
		path, conflicts, chosen := s.Path(), PhotoAuthoredFields(0), ""
		if path == w.ConflictPath {
			conflicts, chosen = w.Conflicts, choice
		}
		managed := s.Head == nil && w.History != nil && w.History.WrittenVersion == w.Sidecar.Node.CurrentVersionID
		prior := plan.carried(i > 0 || w.fold != nil)
		plan.Packet = s.Packet
		write = plan.compare(s, managed, conflicts, chosen)
		if plan.Conflicts != 0 {
			plan.ConflictPath, plan.Fold = path, prior
			break
		}
	}
	return plan, write
}

// newPhotoXMPPlan starts from the catalog the compared copies left, if a conflict kept it.
func newPhotoXMPPlan(w PhotoXMPWork, choice string) PhotoXMPPlan {
	values, keywords := w.catalog()
	plan := PhotoXMPPlan{Work: w, Choice: choice, Values: values, Keywords: PhotoXMPKeywords(keywords), Outside: slices.Clone(w.outside())}
	if w.fold != nil {
		plan.Suppliers, plan.Import = slices.Clone(w.fold.Suppliers), len(w.fold.Suppliers) > 0
	}
	return plan
}

// carried is what a conflict at the next step keeps, or nil when no step before it left anything.
func (p *PhotoXMPPlan) carried(keep bool) *photoXMPFold {
	if !keep {
		return nil
	}
	return &photoXMPFold{p.Values, p.Keywords, slices.Clone(p.Outside), slices.Clone(p.Suppliers), p.Work.token}
}

// supplied records the version a value was imported from; the first is the
// import's primary. It returns the value so the caller keeps its line.
func supplied[T any](p *PhotoXMPPlan, s PhotoXMPStep, value T) T {
	id := p.Work.Sidecar.Node.CurrentVersionID
	if s.Head != nil {
		id = s.Head.VersionID
	}
	if !slices.Contains(p.Suppliers, id) {
		p.Suppliers = append(p.Suppliers, id)
	}
	p.Import = true
	return value
}

// Path is the outside source the step compares, empty for Docbank's own versions.
func (s PhotoXMPStep) Path() string {
	if s.Head == nil {
		return ""
	}
	return s.Head.Path
}

// compare merges one step into the plan. A source without a packet of its own
// is a first encounter: a divergent present value is a conflict, and absent
// properties keep catalog decisions. The mask it returns is what the step's
// packet needs written; only the last step's packet is the sidecar's.
func (p *PhotoXMPPlan) compare(s PhotoXMPStep, managed bool, conflicts PhotoAuthoredFields, choice string) PhotoAuthoredFields {
	w, incoming, catalog, keywords := p.Work, s.Packet, p.Values, p.Keywords
	base := PhotoXMPSnapshot{Packet: PhotoXMPPacket{Keywords: []string{}}, Keywords: []string{}}
	known := s.base != nil
	if known {
		base = *s.base
	}
	outside := base
	outside.Packet.Keywords = slices.Clone(base.Packet.Keywords)
	var write PhotoAuthoredFields
	for _, field := range photoAuthoredFields {
		if incoming.Ignored&field.bit != 0 {
			if choice == "catalog" && conflicts&field.bit != 0 || w.History != nil && !field.sameState(&catalog, &w.History.Settled.Catalog) || w.History == nil && catalog.Confirmed&field.bit != 0 {
				write |= field.bit
			}
			continue
		}
		first := !known || base.Packet.Ignored&field.bit != 0
		chosen := 0
		var accounted bool
		if managed {
			accounted = !first && field.equal(&catalog, &base.Packet.PhotoAuthored)
		} else {
			chosen, accounted = mergePhotoXMPValue(photoXMPComparison{
				equal:       field.equal(&catalog, &incoming.PhotoAuthored),
				samePacket:  !first && field.samePacket(incoming, base.Packet),
				sameCatalog: (known || catalog.Confirmed&field.bit == 0) && field.sameState(&catalog, &base.Catalog),
				absent:      first && (incoming.Present&field.bit == 0 || !field.nonDefault(&incoming.PhotoAuthored)),
				sticky:      conflicts&field.bit != 0,
				choice:      choice,
			})
		}
		selected := catalog
		if chosen == 1 {
			selected = incoming.PhotoAuthored
			p.Values = supplied(p, s, PhotoXMPOverlay(p.Values, selected, field.bit))
			selected = p.Values
		}
		if chosen == 2 {
			p.Conflicts |= field.bit
		}
		if !field.equal(&selected, &incoming.PhotoAuthored) || first && catalog.Confirmed&field.bit != 0 && incoming.Present&field.bit == 0 {
			write |= field.bit
		}
		if accounted {
			outside.Catalog = PhotoXMPOverlay(outside.Catalog, selected, field.bit)
			outside.Catalog.Confirmed = outside.Catalog.Confirmed&^field.bit | selected.Confirmed&field.bit
		}
	}
	first := !known
	chosen := 0
	var accounted bool
	if managed {
		accounted = !first && slices.Equal(keywords, base.Packet.Keywords)
	} else {
		chosen, accounted = mergePhotoXMPValue(photoXMPComparison{
			equal:       slices.Equal(keywords, incoming.Keywords),
			samePacket:  !first && slices.Equal(incoming.Keywords, base.Packet.Keywords),
			sameCatalog: slices.Equal(keywords, base.Keywords),
			absent:      first && len(incoming.Keywords) == 0,
			sticky:      conflicts&PhotoXMPKeywordsBit != 0,
			choice:      choice,
		})
	}
	if chosen == 1 {
		p.Keywords = supplied(p, s, incoming.Keywords)
	}
	if chosen == 2 {
		p.Conflicts |= PhotoXMPKeywordsBit
	}
	if !slices.Equal(p.Keywords, incoming.Keywords) {
		write |= PhotoXMPKeywordsBit
	}
	if !managed {
		outside.Packet = incoming
		if known {
			outside.Packet = incoming.keepUnreadable(base.Packet)
		}
	}
	if accounted {
		outside.Keywords = p.Keywords
	}
	if h := s.Head; h != nil {
		outside.Digest, outside.VersionID, outside.SourceOrientation = h.Digest, h.VersionID, s.baseline.SourceOrientation
		p.Outside = append(slices.DeleteFunc(p.Outside, func(e PhotoXMPSource) bool { return e.Path == h.Path }), PhotoXMPSource{Path: h.Path, PhotoXMPSnapshot: outside})
	}
	return write
}

// A legacy -1 carries the catalog rating in memory, but restoring that numeric
// rating in the packet is still an outside edit.
func (f photoAuthoredField) samePacket(incoming, base PhotoXMPPacket) bool {
	if f.bit == PhotoConfirmedRating && base.LegacyReject && !incoming.LegacyReject {
		return false
	}
	return f.equal(&incoming.PhotoAuthored, &base.PhotoAuthored)
}
