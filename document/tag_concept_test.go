package document

import "testing"

func TestWouldCreateConceptCycle(t *testing.T) {
	edges := map[string][]string{"a": {"b"}, "b": {"c"}}
	if !WouldCreateConceptCycle(edges, "c", "a") || WouldCreateConceptCycle(edges, "a", "d") {
		t.Fatal("cycle check")
	}
}
