package processing

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const photoXMPOrientationField = "Orientation"

type photoXMPProperty struct {
	space, name, collection string
	bit                     store.PhotoAuthoredFields
}

// photoXMPProperties lists the supported properties in the order export writes them.
var photoXMPProperties = [...]photoXMPProperty{
	{xmpBasicNamespace, "Rating", "", store.PhotoConfirmedRating},
	{xmpBasicNamespace, "Label", "", store.PhotoConfirmedLabel},
	{teststripXMPNamespace, "Pick", "", store.PhotoConfirmedFlag},
	{teststripXMPNamespace, "Rotation", "", store.PhotoConfirmedRotation},
	{xmpDublinCoreNamespace, sourceMetadataDescriptionField, "Alt", store.PhotoConfirmedCaption},
	{xmpDublinCoreNamespace, sourceMetadataCreatorField, "Seq", store.PhotoConfirmedCreator},
	{xmpDublinCoreNamespace, sourceMetadataRightsField, "Alt", store.PhotoConfirmedCopyright},
	{xmpDublinCoreNamespace, sourceMetadataSubjectField, "Bag", store.PhotoXMPKeywordsBit},
	{tiffXMPNamespace, photoXMPOrientationField, "", store.PhotoConfirmedRotation},
}

var photoXMPSidecarPrefixes = map[string]string{
	xmpBasicNamespace: "xmp", xmpDublinCoreNamespace: "dc", tiffXMPNamespace: "tiff",
	teststripXMPNamespace: "ts", lightroomXMPNamespace: "lr", rdfNamespace: "rdf",
}

func photoXMPPropertyFor(n xml.Name) photoXMPProperty {
	for _, p := range photoXMPProperties {
		if n.Space == p.space && n.Local == p.name {
			return p
		}
	}
	return photoXMPProperty{}
}

func photoXMPLightroomKeywords(n xml.Name) bool {
	return n.Space == lightroomXMPNamespace && (n.Local == "hierarchicalSubject" || n.Local == "weightedFlatSubject")
}

func photoXMPPropertyText(p photoXMPProperty, v store.PhotoAuthored, source int) string {
	if p.name == photoXMPOrientationField {
		return strconv.Itoa(store.PhotoXMPOrientation(source, v.Rotation))
	}
	return store.PhotoAuthoredText(v, p.bit)
}

func photoXMPWriteText(p photoXMPProperty, v store.PhotoAuthored, source int) string {
	if v.Flag == "reject" {
		if p.bit == store.PhotoConfirmedRating {
			return "-1"
		}
		if p.bit == store.PhotoConfirmedFlag {
			return ""
		}
	}
	return photoXMPPropertyText(p, v, source)
}

func mergePhotoXMPSidecar(ctx context.Context, parsed photoSidecarPacket, input store.PhotoExportInput, source int, mask store.PhotoAuthoredFields) ([]byte, photoSidecarPacket, error) {
	if len(parsed.tokens) == 0 {
		var claim strings.Builder
		if parsed.SidecarForExtension != "" {
			claim.WriteString(` xmlns:photoshop="` + photoshopXMPNamespace + `" photoshop:SidecarForExtension="`)
			_ = xml.EscapeText(&claim, []byte(parsed.SidecarForExtension))
			claim.WriteString(`"`)
		}
		var err error
		parsed, err = readPhotoSidecar(ctx, []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description`+claim.String()+`/></rdf:RDF></x:xmpmeta>`), photoSidecarEditing)
		if err != nil {
			return nil, parsed, err
		}
	}
	text := func(p photoXMPProperty) string {
		return photoXMPWriteText(p, input.Authored, source)
	}
	var out bytes.Buffer
	encoder := &photoXMPEncoder{encoder: xml.NewEncoder(&out), prefixes: photoXMPSidecarPrefixes}
	written := map[xml.Name]bool{}
	write := func(p photoXMPProperty) error {
		name := xml.Name{Space: p.space, Local: p.name}
		if written[name] || p.bit&mask == 0 {
			return nil
		}
		written[name] = true
		value := text(p)
		if handled, err := parsed.rewrite(encoder.EncodeToken, p, value, input.Keywords); handled || err != nil {
			return err
		}
		return writePhotoXMPProperty(encoder, p.space, p.name, value, p.collection, input.Keywords)
	}
	for i := 0; i < len(parsed.tokens); i++ {
		token := parsed.tokens[i]
		if parsed.descriptions[i] {
			t, _ := token.(xml.StartElement)
			attrs := []xml.Attr{}
			for _, a := range t.Attr {
				if photoXMPPropertyFor(a.Name).bit&mask != 0 || mask&store.PhotoXMPKeywordsBit != 0 && photoXMPLightroomKeywords(a.Name) && !photoXMPLightroomRetained(a.Name, a.Value, input.Keywords) {
					continue
				}
				attrs = append(attrs, a)
			}
			for _, property := range photoXMPProperties {
				if property.collection != "" || property.bit&mask == 0 {
					continue
				}
				name := xml.Name{Space: property.space, Local: property.name}
				if written[name] {
					continue
				}
				written[name] = true
				if value := text(property); value != "" {
					attrs = append(attrs, xml.Attr{Name: name, Value: value})
				}
			}
			t.Attr = attrs
			token = t
		}
		if end, ok := parsed.properties[i]; ok {
			t, _ := token.(xml.StartElement)
			p := photoXMPPropertyFor(t.Name)
			alias := mask&store.PhotoXMPKeywordsBit != 0 && photoXMPLightroomKeywords(t.Name)
			if p.bit&mask != 0 || alias {
				if alias {
					if err := editPhotoXMPCollection(encoder.EncodeToken, t, parsed.tokens[i:end], p.collection, text(p), input.Keywords, alias); err != nil {
						return nil, parsed, err
					}
				} else if err := write(p); err != nil {
					return nil, parsed, err
				}
				i = end - 1
				continue
			}
		}
		if i == parsed.firstDescription {
			for _, p := range photoXMPProperties {
				if err := write(p); err != nil {
					return nil, parsed, err
				}
			}
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, parsed, err
		}
	}
	if err := encoder.encoder.Flush(); err != nil {
		return nil, parsed, fmt.Errorf("flush XMP packet: %w", err)
	}
	if out.Len() > maxPhotoSidecarBytes {
		return nil, parsed, bundle.ErrLimit
	}
	parsed, err := readPhotoSidecar(ctx, out.Bytes(), photoSidecarEditing)
	return out.Bytes(), parsed, errors.Join(err, parsed.refusal)
}

func SyncPhotoXMPBackground(ctx context.Context, ing *ingest.Ingester, id string) error {
	w, run, err := ing.Store.ClassifyPhotoXMP(ctx, id)
	if err != nil || !run {
		return err
	}
	_, err = syncPhotoXMP(ctx, ing, w, "")
	return err
}

func ResolvePhotoXMP(ctx context.Context, ing *ingest.Ingester, id, receipt, choice string) (*store.PhotoAuthoredReceipt, error) {
	w, err := ing.Store.PhotoXMPConflict(ctx, id, receipt)
	if err != nil {
		return nil, err
	}
	return syncPhotoXMP(ctx, ing, w, choice)
}

// readPhotoXMPSyncPacket separates unavailable bytes from a packet refusal.
// Both the current packet and a retained outside ancestor use the same limits.
func readPhotoXMPSyncPacket(ctx context.Context, ing *ingest.Ingester, hash string, size int64) (photoSidecarPacket, []byte, error) {
	p := photoSidecarPacket{Keywords: []string{}}
	if size > maxPhotoSidecarBytes {
		p.refusal = fmt.Errorf("XMP sidecar exceeds %d bytes", maxPhotoSidecarBytes)
		return p, nil, nil
	}
	reader, _, err := ing.Blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return p, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxPhotoSidecarBytes+1))
	err = errors.Join(err, reader.Close())
	if err != nil {
		return p, nil, err
	}
	p, err = readPhotoSidecar(ctx, data, photoSidecarEditing)
	if err != nil {
		p.Ignored |= store.PhotoConfirmedRotation
	}
	p.refusal = errors.Join(err, p.refusal)
	return p, data, nil
}

// syncPhotoXMP checks that a choice can read the conflicting copy's value, whatever the newer copies hold.
func syncPhotoXMP(ctx context.Context, ing *ingest.Ingester, w store.PhotoXMPWork, choice string) (*store.PhotoAuthoredReceipt, error) {
	refusal := func(err error) (*store.PhotoAuthoredReceipt, error) {
		if choice != "" {
			if errors.Is(err, bundle.ErrLimit) {
				err = fmt.Errorf("XMP output exceeds %d bytes: %w", maxPhotoSidecarBytes, err)
			}
			return nil, fmt.Errorf("%w: %w. Correct the sidecar or catalog metadata and retry", store.ErrInvalidPhotoAsset, err)
		}
		return ing.PublishPhotoXMP(ctx, store.PhotoXMPPlan{Work: w, Choice: choice, Refusal: err.Error()}, nil)
	}
	unavailable := func(err error) (*store.PhotoAuthoredReceipt, error) {
		_, publishErr := ing.PublishPhotoXMP(ctx, store.PhotoXMPPlan{Work: w, State: store.PhotoXMPStatePending, Refusal: "Sidecar bytes are unavailable. Restore them; sync retries automatically."}, nil)
		return nil, errors.Join(err, publishErr)
	}

	if err := store.ValidatePhotoXMPAuthored(w.Original.Authored()); err != nil {
		return refusal(err)
	}
	var data []byte
	p := photoSidecarPacket{Keywords: []string{}}
	if w.Sidecar != nil {
		var err error
		p, data, err = readPhotoXMPSyncPacket(ctx, ing, w.Sidecar.Node.BlobHash, w.Sidecar.Node.Size)
		if err != nil {
			return unavailable(err)
		}
		if p.refusal != nil {
			return refusal(fmt.Errorf("%w. Correct the sidecar or detach it to resume sync", p.refusal))
		}
		// Names and extension claims pair a sidecar only until its first sync; the saved attachment then stands.
		if w.History == nil {
			if err = ing.Store.CheckPhotoXMPPairing(ctx, w, p.SidecarForExtension); err != nil {
				if errors.Is(err, store.ErrInvalidPhotoAsset) {
					return refusal(err)
				}
				return nil, err
			}
		}
	}
	steps := w.Steps()
	for i := range steps {
		step, packet := &steps[i], p
		if step.Head != nil && step.Head.Digest != w.Sidecar.Node.BlobHash {
			var err error
			if packet, _, err = readPhotoXMPSyncPacket(ctx, ing, step.Head.Digest, step.Head.Size); err != nil {
				return unavailable(err)
			}
			if packet.refusal != nil {
				return refusal(fmt.Errorf("%w. Correct the sidecar or detach it to resume sync", packet.refusal))
			}
		}
		if step.Head != nil && w.History == nil {
			if err := ing.Store.CheckPhotoXMPClaim(ctx, w, step.Head.Path, packet.SidecarForExtension); err != nil {
				return refusal(err)
			}
		}
		if step.Path() == w.ConflictPath && packet.Ignored&w.Conflicts != 0 {
			return refusal(errors.New("conflicting XMP value is unreadable; correct the sidecar and retry. Correct the sidecar or detach it to resume sync"))
		}
		step.Packet = packet.PhotoXMPPacket
		base, source := step.Baseline()
		if err := resolvePhotoXMPRotation(&step.Packet, base, source); err != nil {
			return refusal(err)
		}
	}
	plan := store.PhotoXMPPlan{Work: w, Choice: choice, Values: w.Original.Authored(), Keywords: w.Keywords}
	var mask store.PhotoAuthoredFields
	if w.Sidecar != nil {
		p.PhotoXMPPacket = steps[len(steps)-1].Packet
		plan, mask = store.PlanPhotoXMP(w, steps, choice)
	}
	if plan.Conflicts != 0 {
		return ing.PublishPhotoXMP(ctx, plan, nil)
	}
	if p.Ignored&store.PhotoConfirmedRotation == 0 && (p.Rotation != nil || p.Orientation != 0) {
		if p.Rotation == nil && plan.Values.Rotation != 0 || p.Rotation != nil && *p.Rotation != plan.Values.Rotation || p.Orientation != store.PhotoXMPOrientation(w.Orientation, plan.Values.Rotation) {
			mask |= store.PhotoConfirmedRotation
		}
	}
	if w.Sidecar == nil {
		plan.Outside = nil
		// Checkout may deliver this sidecar by stem, so it names its original as Adobe and Teststrip expect.
		p.SidecarForExtension = w.ClaimExtension
		// First creation includes only confirmed values that differ from their defaults.
		mask = store.PhotoXMPChangedFields(plan.Values, store.PhotoAuthored{Confirmed: plan.Values.Confirmed}) & plan.Values.Confirmed
		if w.Removal != nil {
			mask |= store.PhotoXMPChangedFields(plan.Values, w.Removal.Catalog)
			if w.KeywordFingerprint != w.Removal.KeywordFingerprint {
				mask |= store.PhotoXMPKeywordsBit
			}
		}
		if len(plan.Keywords) > 0 {
			mask |= store.PhotoXMPKeywordsBit
		}
		if mask == 0 {
			plan.State = store.PhotoXMPStateNone
			return ing.PublishPhotoXMP(ctx, plan, nil)
		}
	}
	if mask&store.PhotoConfirmedFlag != 0 && (plan.Values.Flag == "reject" || p.LegacyReject) || p.LegacyReject && mask&store.PhotoConfirmedRating != 0 {
		mask |= store.PhotoConfirmedRating | store.PhotoConfirmedFlag
	}
	plan.Write = mask != 0
	if plan.Write {
		merged, written, err := mergePhotoXMPSidecar(ctx, p,
			store.PhotoExportInput{Authored: plan.Values, Keywords: plan.Keywords}, w.Orientation, mask)
		if err != nil {
			return refusal(err)
		}
		data = merged
		plan.Packet = written.PhotoXMPPacket
		if mask&store.PhotoConfirmedRotation == 0 {
			plan.Packet.PhotoAuthored.Rotation = plan.Values.Rotation
		} else if err = resolvePhotoXMPRotation(&plan.Packet, store.PhotoXMPPacket{}, w.Orientation); err != nil {
			return refusal(err)
		}
	}
	return ing.PublishPhotoXMP(ctx, plan, data)
}

// resolvePhotoXMPRotation reads ts:Rotation and tiff:Orientation against the
// base: the one that changed sets the rotation, and removing either clears it.
func resolvePhotoXMPRotation(p *store.PhotoXMPPacket, base store.PhotoXMPPacket, source int) error {
	if p.Ignored&store.PhotoConfirmedRotation != 0 {
		return nil
	}
	tsChanged := (p.Rotation == nil) != (base.Rotation == nil) || p.Rotation != nil && base.Rotation != nil && *p.Rotation != *base.Rotation
	removedTS := p.Rotation == nil && base.Rotation != nil
	removedTIFF := p.Orientation == 0 && base.Orientation != 0
	if p.Orientation == 0 {
		if removedTS || removedTIFF && (p.Rotation == nil || !tsChanged) {
			p.PhotoAuthored.Rotation = 0
		}
		return nil
	}
	rotation := -1
	for _, r := range []int{0, 90, 180, 270} {
		if store.PhotoXMPOrientation(source, r) == p.Orientation {
			rotation = r
		}
	}
	if rotation < 0 {
		return errors.New("XMP Orientation cannot be composed with the source orientation")
	}
	before := base.Orientation
	if before == 0 {
		before = store.PhotoXMPOrientation(source, 0)
	}
	tiffChanged := p.Orientation != before
	if removedTS && !tiffChanged {
		p.PhotoAuthored.Rotation = 0
		return nil
	}
	if tsChanged && p.Rotation != nil {
		if tiffChanged && rotation != *p.Rotation {
			return errors.New("XMP Rotation and Orientation disagree")
		}
		rotation = *p.Rotation
	} else if !tiffChanged {
		rotation = base.PhotoAuthored.Rotation
	}
	p.PhotoAuthored.Rotation = rotation
	return nil
}
