package report

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestVisibilityIdentitiesIncludesBothRelationEndpointsOnce(t *testing.T) {
	selected := Identity{NodeID: 1, VersionID: "selected", SHA256: strings.Repeat("a", 64)}
	parent := Identity{NodeID: 2, VersionID: "parent", SHA256: strings.Repeat("b", 64)}
	child := Identity{NodeID: 3, VersionID: "child", SHA256: strings.Repeat("c", 64)}
	frame := Frame{Members: []Member{{Identity: selected}}, Relations: []Relation{
		{Parent: parent, Child: selected}, {Parent: selected, Child: child},
	}}
	got, err := VisibilityIdentities(frame)
	if err != nil || !reflect.DeepEqual(got, []Identity{selected, parent, child}) {
		t.Fatalf("visibility identities=%+v err=%v", got, err)
	}
}

func TestVisibilityIdentitiesBoundsRelationDependencies(t *testing.T) {
	parent := Identity{NodeID: 1, VersionID: "synthetic-parent", SHA256: strings.Repeat("a", 64)}
	frame := Frame{Members: []Member{{Identity: parent}}, Relations: make([]Relation, 50000)}
	for i := range frame.Relations {
		frame.Relations[i] = Relation{Parent: parent, Child: Identity{
			NodeID: int64(i + 2), VersionID: "synthetic-child", SHA256: strings.Repeat("b", 64),
		}}
	}
	if _, err := VisibilityIdentities(frame); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("visibility dependency limit: %v", err)
	}
}
