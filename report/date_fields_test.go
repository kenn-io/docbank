package report

import (
	"context"
	"strings"
	"testing"
)

func TestDateFieldsMapNativeAuthorityFromNamespace(t *testing.T) {
	root := NewBudget(1 << 20)
	defer func() { _ = root.Close() }()
	identity := Identity{NodeID: 1, VersionID: "v1", SHA256: strings.Repeat("a", 64)}
	fields := []RawDateField{
		{Document: identity, Namespace: "email", SourceField: "Date", Key: "email.sent", Raw: "Tue, 2 Jan 2024 03:04:05 -0700", Normalized: "2024-01-02T03:04:05-07:00", Precision: "second", GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64)},
		{Document: identity, Namespace: "pdf.info", SourceField: "CreationDate", Key: "created", Raw: "D:20240102", Normalized: "2024-01-02", Precision: "date", GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64)},
		{Document: identity, Namespace: "office.core", SourceField: "created", Key: "created", Raw: "2024-01-02", Normalized: "2024-01-02", Precision: "date", GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64)},
		{Document: identity, Namespace: "image.exif", SourceField: "DateTimeOriginal", Key: "created", Raw: "2024:01:02 03:04:05", Normalized: "2024-01-02T03:04:05", Precision: "second", GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64)},
		{Document: identity, Namespace: "custom", SourceField: "created", Key: "created", Raw: "2020-01-01", Normalized: "2020-01-01", Precision: "date", GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64)},
	}
	got, err := AdaptDateFields(context.Background(), root, fields)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []string{"sent", "created", "created", "captured", "unclassified"}
	wantClasses := []string{"native", "native", "native", "native", "source_metadata"}
	if len(got) != len(fields) {
		t.Fatalf("candidate count=%d", len(got))
	}
	for i, candidate := range got {
		if candidate.Role != wantRoles[i] || candidate.SourceClass != wantClasses[i] || candidate.ID == "" || candidate.Raw != fields[i].Raw {
			t.Fatalf("candidate %d=%+v", i, candidate)
		}
	}
}

func TestDateFieldsKeepRejectedRawTimestamp(t *testing.T) {
	root := NewBudget(1 << 20)
	defer func() { _ = root.Close() }()
	got, err := AdaptDateFields(context.Background(), root, []RawDateField{{
		Document:  Identity{NodeID: 1, VersionID: "v1"},
		Namespace: "pdf.info", SourceField: "CreationDate", Key: "created",
		Raw: "D:20240230", Normalized: "2024-02-30", Precision: "date",
		GenerationID: "g1", GenerationSHA256: strings.Repeat("b", 64),
	}})
	if err != nil || len(got) != 1 || got[0].Rejection == "" || got[0].Raw != "D:20240230" {
		t.Fatalf("candidates=%+v err=%v", got, err)
	}
}

func TestDateFieldsRejectSensitiveUnfilteredEvidence(t *testing.T) {
	root := NewBudget(1 << 20)
	defer func() { _ = root.Close() }()
	_, err := AdaptDateFields(context.Background(), root, []RawDateField{{
		Namespace: "email", SourceField: "Date", Key: "email.sent", Raw: "private", Sensitive: true,
	}})
	if err == nil {
		t.Fatal("accepted date evidence that had not passed disclosure filtering")
	}
}
