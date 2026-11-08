package api

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// PublishedNoteSelector opts into current published sources. Empty publication
// selects every active namespace; an omitted selector preserves archive reads.
type PublishedNoteSelector struct {
	Publication string `json:"publication" required:"false" maxLength:"64"`
}

// Huma's wrapper preserves the difference between an absent object and an
// explicitly supplied empty publication in deepObject query parameters.
type publishedNoteParameter struct {
	Value PublishedNoteSelector
	Set   bool
}

func (p *publishedNoteParameter) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[PublishedNoteSelector](), true, "")
}
func (p *publishedNoteParameter) Receiver() reflect.Value    { return reflect.ValueOf(&p.Value).Elem() }
func (p *publishedNoteParameter) OnParamSet(set bool, _ any) { p.Set = set }
func (p *publishedNoteParameter) selector() *PublishedNoteSelector {
	if !p.Set {
		return nil
	}
	return &p.Value
}

func publishedNoteStoreSelector(selector *PublishedNoteSelector) *store.PublishedNoteSelector {
	if selector == nil {
		return nil
	}
	return &store.PublishedNoteSelector{Publication: selector.Publication}
}
func publishedNoteWireSelector(selector *store.PublishedNoteSelector) *PublishedNoteSelector {
	if selector == nil {
		return nil
	}
	return &PublishedNoteSelector{Publication: selector.Publication}
}
func publishedNoteWireIdentity(identity *store.PublishedNoteIdentity) *PublishedNoteIdentity {
	if identity == nil {
		return nil
	}
	return new(PublishedNoteIdentity(*identity))
}
