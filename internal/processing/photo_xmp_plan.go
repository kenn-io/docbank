package processing

import (
	"encoding/xml"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/store"
)

const tiffXMPNamespace = "http://ns.adobe.com/tiff/1.0/"

type photoXMPProperty struct {
	space, name, collection, label string
	bit                            store.PhotoAuthoredFields
}

var photoXMPProperties = [...]photoXMPProperty{
	{xmpBasicNamespace, "Rating", "", "Rating", store.PhotoConfirmedRating},
	{teststripXMPNamespace, "Pick", "", "Flag", store.PhotoConfirmedFlag},
	{xmpBasicNamespace, "Label", "", "Label", store.PhotoConfirmedLabel},
	{xmpDublinCoreNamespace, "description", "Alt", "Caption", store.PhotoConfirmedCaption},
	{xmpDublinCoreNamespace, "creator", "Seq", "Creator", store.PhotoConfirmedCreator},
	{xmpDublinCoreNamespace, "rights", "Alt", "Copyright", store.PhotoConfirmedCopyright},
	{tiffXMPNamespace, "Orientation", "", "Rotation", store.PhotoConfirmedRotation},
	{xmpDublinCoreNamespace, "subject", "Bag", "Keywords", store.PhotoXMPKeywordsBit},
}

func photoXMPPropertyFor(n xml.Name) photoXMPProperty {
	if n == (xml.Name{Space: teststripXMPNamespace, Local: "Rotation"}) {
		return photoXMPProperty{space: n.Space, name: n.Local, bit: store.PhotoConfirmedRotation}
	}
	for _, p := range &photoXMPProperties {
		if n.Space == p.space && n.Local == p.name {
			return p
		}
	}
	return photoXMPProperty{}
}

type photoXMPPlan struct {
	packet         store.PhotoXMPPacket
	write, imports store.PhotoAuthoredFields
	conflict       store.PhotoAuthoredFields
}

func photoXMPPresent(p store.PhotoXMPPacket, bit store.PhotoAuthoredFields) bool {
	if bit == store.PhotoXMPKeywordsBit {
		return len(p.Keywords) > 0
	}
	return p.Values.Confirmed&bit != 0
}
func photoXMPEqual(a, b store.PhotoXMPPacket, bit store.PhotoAuthoredFields) bool {
	ap, bp := photoXMPPresent(a, bit), photoXMPPresent(b, bit)
	if ap != bp {
		return false
	}
	if !ap {
		return true
	}
	if bit == store.PhotoXMPKeywordsBit {
		return slices.Equal(a.Keywords, b.Keywords)
	}
	if bit == store.PhotoConfirmedRotation {
		return a.Values.Rotation == b.Values.Rotation
	}
	return store.PhotoXMPValuesEqual(a.Values, b.Values, bit)
}
func photoXMPCopy(a *store.PhotoXMPPacket, b store.PhotoXMPPacket, bit store.PhotoAuthoredFields) {
	if bit == store.PhotoXMPKeywordsBit {
		a.Keywords = b.Keywords
		return
	}
	a.Values = store.PhotoXMPOverlay(a.Values, b.Values, bit)
	a.Values.Confirmed = a.Values.Confirmed&^bit | b.Values.Confirmed&bit
	if bit == store.PhotoConfirmedRotation {
		a.Orientation = b.Orientation
	}
}

func effectivePhotoXMPSidecar(base, catalog, sidecar store.PhotoXMPPacket) store.PhotoXMPPacket {
	for _, p := range &photoXMPProperties {
		if sidecar.Ignored&p.bit != 0 || !photoXMPPresent(sidecar, p.bit) && !photoXMPPresent(base, p.bit) {
			photoXMPCopy(&sidecar, catalog, p.bit)
		}
	}
	return sidecar
}

// planPhotoXMP applies the same three-way decision independently to each property.
func planPhotoXMP(base, priorCatalog, catalog, sidecar store.PhotoXMPPacket, choice string) photoXMPPlan {
	sidecar.Values.Confirmed &= store.PhotoAuthoredNonDefault(sidecar.Values)
	raw := sidecar
	sidecar = effectivePhotoXMPSidecar(base, catalog, sidecar)
	if priorCatalog.Keywords != nil && sidecar.Ignored&store.PhotoXMPKeywordsBit == 0 {
		sidecar.Keywords = raw.Keywords
	}
	result := photoXMPPlan{packet: catalog}
	result.packet.Keywords = slices.Clone(catalog.Keywords)
	result.packet.Ignored = sidecar.Ignored
	for _, property := range &photoXMPProperties {
		bit := property.bit
		if bit == store.PhotoXMPKeywordsBit && priorCatalog.Keywords != nil && sidecar.Ignored&bit == 0 {
			for _, tag := range store.PhotoXMPKeywords(append(append(append(slices.Clone(base.Keywords), priorCatalog.Keywords...), catalog.Keywords...), sidecar.Keywords...)) {
				b, p, c, x := slices.Contains(base.Keywords, tag), slices.Contains(priorCatalog.Keywords, tag), slices.Contains(catalog.Keywords, tag), slices.Contains(sidecar.Keywords, tag)
				value := c
				switch {
				case c == x, x == b:
				case c == p, choice == "sidecar":
					value = x
				case choice == "catalog":
				default:
					result.conflict |= bit
				}
				if value != c {
					result.imports |= bit
				}
				if value != x {
					result.write |= bit
				}
				if value {
					result.packet.Keywords = append(result.packet.Keywords, tag)
				} else {
					result.packet.Keywords = slices.DeleteFunc(result.packet.Keywords, func(v string) bool { return v == tag })
				}
			}
			result.packet.Keywords = store.PhotoXMPKeywords(result.packet.Keywords)
			continue
		}
		equal := func(a, b store.PhotoXMPPacket) bool { return photoXMPEqual(a, b, bit) }
		ignored := sidecar.Ignored&bit != 0
		if !ignored && !photoXMPPresent(raw, bit) && !photoXMPPresent(base, bit) {
			if photoXMPPresent(catalog, bit) {
				result.write |= bit
			}
			continue
		}
		if ignored && (equal(catalog, priorCatalog) || !photoXMPPresent(base, bit) && base.Ignored&bit == 0) {
			continue
		}
		if ignored && !photoXMPPresent(priorCatalog, bit) && !photoXMPPresent(catalog, bit) {
			continue
		}
		if ignored {
			result.write |= bit
			result.packet.Ignored &^= bit
			continue
		}
		switch {
		case equal(catalog, sidecar) || bit != store.PhotoXMPKeywordsBit && store.PhotoXMPValuesEqual(catalog.Values, sidecar.Values, bit):
		case equal(sidecar, base):
			result.write |= bit
		case equal(catalog, priorCatalog):
			// Editors emit defaults for fields the photographer has never decided.
			if !photoXMPPresent(base, bit) && (bit == store.PhotoXMPKeywordsBit && len(sidecar.Keywords) == 0 || bit != store.PhotoXMPKeywordsBit && store.PhotoAuthoredNonDefault(sidecar.Values)&bit == 0) {
				continue
			}
			result.imports |= bit
			photoXMPCopy(&result.packet, sidecar, bit)
		case choice == "catalog":
			result.write |= bit
		case choice == "sidecar":
			result.imports |= bit
			photoXMPCopy(&result.packet, sidecar, bit)
		default:
			result.conflict |= bit
		}
	}
	return result
}

func resolvePhotoXMPRotation(sidecar *store.PhotoXMPPacket, base store.PhotoXMPPacket, source int) {
	// Preserve unreadable rotation evidence before considering either encoding.
	if sidecar.Ignored&store.PhotoConfirmedRotation != 0 {
		sidecar.Values.Confirmed &^= store.PhotoConfirmedRotation
		return
	}
	if sidecar.Orientation == 0 {
		if sidecar.Values.Rotation == 0 {
			sidecar.Values.Confirmed &^= store.PhotoConfirmedRotation
		}
		return
	}
	tiffRotation := -1
	for _, r := range []int{0, 90, 180, 270} {
		if store.PhotoXMPOrientation(source, r) == sidecar.Orientation {
			tiffRotation = r
			break
		}
	}
	if tiffRotation < 0 {
		sidecar.Ignored |= store.PhotoConfirmedRotation
		sidecar.Values.Confirmed &^= store.PhotoConfirmedRotation
		return
	}
	tiffChanged := sidecar.Orientation != base.Orientation
	tsRotation := 0
	if sidecar.Rotation != nil {
		tsRotation = *sidecar.Rotation
	}
	tsChanged := (sidecar.Rotation == nil) != (base.Rotation == nil) || sidecar.Rotation != nil && base.Rotation != nil && tsRotation != *base.Rotation
	rotation := tiffRotation
	if !tsChanged && !tiffChanged {
		rotation = base.Values.Rotation
	}
	if tsChanged && !tiffChanged {
		rotation = tsRotation
	}
	if tsChanged && tiffChanged && tiffRotation != tsRotation {
		sidecar.Ignored |= store.PhotoConfirmedRotation
		sidecar.Values.Confirmed &^= store.PhotoConfirmedRotation
		return
	}
	sidecar.Values.Rotation = rotation
	sidecar.Values.Confirmed &^= store.PhotoConfirmedRotation
	if rotation != 0 {
		sidecar.Values.Confirmed |= store.PhotoConfirmedRotation
	}
	sidecar.Ignored &^= store.PhotoConfirmedRotation
}

func photoXMPConflictRows(catalog, sidecar store.PhotoXMPPacket, conflicts store.PhotoAuthoredFields) []store.PhotoXMPRow {
	rows := []store.PhotoXMPRow{}
	a, b := photoXMPPropertyValues(catalog.Values, catalog.Values.Rotation), photoXMPPropertyValues(sidecar.Values, sidecar.Values.Rotation)
	for _, p := range &photoXMPProperties {
		av, bv := a[p.bit], b[p.bit]
		if p.bit == store.PhotoXMPKeywordsBit {
			av, bv = strings.Join(catalog.Keywords, ", "), strings.Join(sidecar.Keywords, ", ")
		}
		if conflicts&p.bit != 0 {
			rows = append(rows, store.PhotoXMPRow{Field: p.label, Catalog: av, Sidecar: bv})
		}
	}
	return rows
}
