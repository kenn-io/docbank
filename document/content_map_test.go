package document

import (
	"errors"
	"testing"
)

func TestValidateMapPins(t *testing.T) {
	if ValidateMapPins([]string{"a"}, []string{"a"}) == nil {
		t.Fatal("contradictory pin")
	}
	if err := ValidateMapPins([]string{"a"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
}

func TestContentMapPinModesAndIdentity(t *testing.T) {
	const doc = "00000000-0000-4000-8000-000000000001"
	const version = "00000000-0000-4000-8000-000000000002"
	for _, pin := range []ContentMapPin{
		{DocumentUID: doc, Mode: MapPinVersionPinned},
		{DocumentUID: doc, Mode: MapPinFollowCurrent, ContentVersionID: version},
		{DocumentUID: doc, Mode: "unknown"},
	} {
		if _, err := ContentMapPinIdentity(pin); !errors.Is(err, ErrInvalidContentMap) {
			t.Fatalf("pin %+v: expected invalid content map, got %v", pin, err)
		}
	}
	if _, err := ContentMapPinIdentity(ContentMapPin{DocumentUID: doc, Mode: MapPinVersionPinned, ContentVersionID: version}); err != nil {
		t.Fatal(err)
	}
	if _, err := ContentMapPinIdentity(ContentMapPin{DocumentUID: doc, Mode: MapPinFollowCurrent}); err != nil {
		t.Fatal(err)
	}
}
