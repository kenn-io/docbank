package document

import "testing"

func TestSameVectorSpace(t *testing.T) {
	if SameVectorSpace("", "") || SameVectorSpace("model-a", "model-b") {
		t.Fatal("incompatible vectors accepted")
	}
	if !SameVectorSpace("recipe-a", "recipe-a") {
		t.Fatal("compatible rejected")
	}
}

func TestLocateConnectionInputBindsExactUnitAndUTF8Range(t *testing.T) {
	body := []byte("α beta\nother text")
	navigation := []RenditionNavigationEntryV1{{Key: "first", Byte: 0}, {Key: "second", Byte: len("α beta\n")}}
	keys := []string{"first", "second"}
	input := GeneratedEmbeddingInput{Content: "beta", SourceSpan: ChunkSpan{UnitIndex: 0, CharStart: 2, CharEnd: 6}}
	start, end, ok := LocateConnectionInput(body, navigation, keys, input)
	if !ok || string(body[start:end]) != "beta" {
		t.Fatalf("exact input not located: %d:%d, ok=%t", start, end, ok)
	}
	input.SourceSpan.UnitIndex = 1
	if _, _, ok := LocateConnectionInput(body, navigation, keys, input); ok {
		t.Fatal("text from another unit was accepted")
	}
	input.SourceSpan.UnitIndex = 0
	input.Content = "different"
	if _, _, ok := LocateConnectionInput(body, navigation, keys, input); ok {
		t.Fatal("mismatched quote was accepted")
	}
}
