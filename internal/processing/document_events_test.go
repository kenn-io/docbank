package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestF10CaptureUsesSourceFieldNotAnInventedKey(t *testing.T) {
	for _, value := range []struct{ namespace, source string }{
		{"image.exif", "DateTimeOriginal"},
		{"media.id3", "TDRC"},
		{"media.container", "mvhd.CreationTime"},
	} {
		kind, ok := metadataDateKind(document.SourceMetadataFieldV1{
			Key: "created", Namespace: value.namespace, SourceField: value.source,
		})
		require.True(t, ok)
		require.Equal(t, document.DateKind("captured"), kind)
	}
	kind, ok := metadataDateKind(document.SourceMetadataFieldV1{
		Key: "created", Namespace: "pdf.info", SourceField: "CreationDate",
	})
	require.True(t, ok)
	require.Equal(t, document.DateKind("created"), kind)
	_, ok = metadataDateKind(document.SourceMetadataFieldV1{Key: "image.exif.date_time_original"})
	require.False(t, ok)
}

func TestDocumentEventDeriverDescriptor(t *testing.T) {
	sum := sha256.Sum256([]byte(documentEventsDeriverDescriptor))
	require.Equal(t, DocumentEventsDeriverFingerprint, hex.EncodeToString(sum[:]))
	require.Equal(t,
		"docbank-document-events:f10-metadata+content-version+provenance-binding:v1",
		documentEventsDeriverDescriptor,
	)
}

func TestAdaptersPreserveDistinctF10CapturedClaimsAndCalendarComponents(t *testing.T) {
	input := documentEventInput("image/png", []document.SourceMetadataFieldV1{
		timestampField("calendar.event.e000001.start", "calendar", "DTSTART", false,
			"2024-04-05T06:07", "20240405T0607", "minute", "omitted"),
		timestampField("created", "image.exif", "DateTimeOriginal", false,
			"2020-01-02T03:04:05", "2020:01:02 03:04:05", "second", "omitted"),
		timestampField("image.exif.gps_timestamp", "image.exif", "GPSTimeStamp", false,
			"2020-01-02T03:04:06Z", "2020:01:02 03:04:06Z", "second", "utc"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.Equal(t, document.DocumentKind("image"), record.DocumentKind)
	require.Len(t, record.Events, 4)

	bySource := make(map[string]document.DocumentEventV1, len(record.Events))
	for _, event := range record.Events {
		bySource[event.SourceKey] = event
	}
	first := bySource["metadata/meta-generation/created/1"]
	second := bySource["metadata/meta-generation/image.exif.gps_timestamp/2"]
	require.Equal(t, document.DateKind("captured"), first.DateKind)
	require.Equal(t, document.DateKind("captured"), second.DateKind)
	require.NotEqual(t, first.EventID, second.EventID)
	calendar := bySource["metadata/meta-generation/calendar.event.e000001.start/0"]
	require.Equal(t, document.DateKind("started"), calendar.DateKind)
	require.Equal(t, "DTSTART", calendar.SourceKindRaw)
}

func TestAdaptersAnchorActorOnlyF10ClaimsToVaultRecorded(t *testing.T) {
	ada := "Ada Lovelace"
	grace := "Grace Hopper"
	input := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{{
		Key: "creators", Namespace: "xmp", SourceField: "dc:creator",
		Value: document.SourceMetadataValueV1{
			Kind: document.SourceMetadataStringList, Strings: []string{ada, grace},
		},
	}})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.Len(t, record.Events, 1)
	require.Equal(t, document.DateKind("vault_recorded"), record.Events[0].DateKind)
	require.Equal(t, "docbank_observed", record.Events[0].ClaimBasis)
	require.Len(t, record.Events[0].Actors, 2)
	require.Equal(t, "name_alias:ada lovelace", record.Events[0].Actors[0].ActorKey)
	require.Equal(t, ada, record.Events[0].Actors[0].Claim)
	require.Len(t, record.Sources, 2, "actor-only F10 authority remains in the record")
}

func TestAdaptersKeepActorOnlyBccWithoutASafePrimary(t *testing.T) {
	input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
		stringField("email.bcc", "Bcc", true, "hidden@example.test"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	event := requireEventKind(t, record.Events, "vault_recorded")
	require.Len(t, event.Actors, 1)
	require.True(t, event.Actors[0].Sensitive)
	require.Equal(t, "hidden@example.test", event.Actors[0].Address)
	require.Equal(t, []document.DocumentEventPrimaryV1{
		requirePrimary(t, record.Primaries, "full"),
	}, record.Primaries)
}

func TestAdaptersRecoverRawEmailMinutePrecisionAndUnknownZone(t *testing.T) {
	from := "Ada Example <ADA@example.test>"
	bcc := "Hidden <hidden@example.test>"
	input := documentEventInput("message/rfc822", []document.SourceMetadataFieldV1{
		stringField("email.bcc", "Bcc", true, bcc),
		stringField("email.from", "From", false, from),
		timestampField("email.sent", "email", "Date", false,
			"2024-01-02T03:04:00Z", "Tue, 2 Jan 2024 03:04 -0000", "second", "utc"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	sent := requireEventKind(t, record.Events, "sent")
	require.Equal(t, "2024-01-02T03:04", sent.DateValue)
	require.Equal(t, document.EventPrecision("minute"), sent.Precision)
	require.Equal(t, document.EventTimezoneKind("omitted"), sent.TimezoneKind)
	require.Equal(t, "-0000", sent.ZoneText)
	require.Nil(t, sent.OffsetSeconds)
	require.Empty(t, sent.UTCKey)
	require.Len(t, sent.Actors, 2)
	require.Equal(t, document.EventRole("blind_copy"), sent.Actors[0].Role)
	require.True(t, sent.Actors[0].Sensitive)
	require.Equal(t, document.EventRole("sender"), sent.Actors[1].Role)
	require.Equal(t, "email:ADA@example.test", sent.Actors[1].ActorKey)
	require.Equal(t, sent.EventID, requirePrimary(t, record.Primaries, "full").EventID)
	require.Equal(t, requireEventKind(t, record.Events, "vault_recorded").EventID,
		requirePrimary(t, record.Primaries, "safe").EventID)

	named := input
	named.Metadata.Fields[2] = timestampField("email.sent", "email", "Date", false,
		"2024-01-02T03:04:00Z", "Tue, 2 Jan 2024 03:04 XYZ", "second", "utc")
	record, _, err = DeriveDocumentEvents(named)
	require.NoError(t, err)
	sent = requireEventKind(t, record.Events, "sent")
	require.Equal(t, document.EventTimezoneKind("named"), sent.TimezoneKind)
	require.Equal(t, "XYZ", sent.ZoneText)
	require.Empty(t, sent.UTCKey)
}

func TestAdaptersDeriveRetainedRawDatesFromExtractor(t *testing.T) {
	for _, test := range []struct {
		name, mime, payload, key, raw, value, zoneText string
		kind                                           document.DateKind
		precision                                      document.EventPrecision
		zone                                           document.EventTimezoneKind
	}{
		{"email named", "message/rfc822", "From: Ada <ada@example.test>\r\nDate: Tue, 2 Jan 2024 03:04 XYZ\r\n\r\nbody", "email.sent.raw", "Tue, 2 Jan 2024 03:04 XYZ", "2024-01-02T03:04", "XYZ", "sent", "minute", "named"},
		{"email omitted", "message/rfc822", "From: Ada <ada@example.test>\r\nDate: Tue, 2 Jan 2024 03:04\r\n\r\nbody", "email.sent", "Tue, 2 Jan 2024 03:04", "2024-01-02T03:04", "", "sent", "minute", "omitted"},
		{"calendar start", "text/calendar", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=America/New_York:20240102T030405\r\nEND:VEVENT\r\nEND:VCALENDAR", "calendar.start.raw", "20240102T030405", "2024-01-02T03:04:05", "America/New_York", "started", "second", "named"},
		{"calendar end date", "text/calendar", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTEND;VALUE=DATE;TZID=Example/Zone:20240103\r\nEND:VEVENT\r\nEND:VCALENDAR", "calendar.end.raw", "20240103", "2024-01-03", "Example/Zone", "ended", "date", "named"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata, err := ExtractSourceMetadata(t.Context(), sourceMetadataTestSpool(t), []byte(test.payload))
			require.NoError(t, err)
			input := canonicalDocumentEventInput(t, metadata.Fields)
			input.Target.MIMEType = test.mime
			record, _, err := DeriveDocumentEvents(input)
			require.NoError(t, err)
			event := requireEventKind(t, record.Events, test.kind)
			require.Equal(t, test.raw, event.RawValue)
			require.Equal(t, test.value, event.DateValue)
			require.Equal(t, test.precision, event.Precision)
			require.Equal(t, test.zone, event.TimezoneKind)
			require.Equal(t, test.zoneText, event.ZoneText)
			require.Empty(t, event.UTCKey)
			require.Equal(t, "field/"+test.key, event.EvidenceLocator)
		})
	}
}

func TestAdaptersUseRFCEmailYearPivot(t *testing.T) {
	for _, test := range []struct{ rawYear, year string }{{"49", "2049"}, {"50", "1950"}, {"68", "1968"}, {"69", "1969"}} {
		t.Run(test.rawYear, func(t *testing.T) {
			input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
				timestampField("email.sent", "email", "Date", false,
					"2068-01-02T03:04:05Z", "2 Jan "+test.rawYear+" 03:04 +0000", "second", "utc"),
			})
			record, _, err := DeriveDocumentEvents(input)
			require.NoError(t, err)
			require.Equal(t, test.year+"-01-02T03:04", requireEventKind(t, record.Events, "sent").DateValue)
		})
	}
}

func TestAdaptersClassifyUnindexableProvenanceTimeAsUnavailable(t *testing.T) {
	input := documentEventInput("text/plain", nil)
	bound := boundDocumentEventInput(input.Target.ContentVersionID, 0)
	bound.OriginalMTime = new("0000-01-02T03:04:05Z")
	input.Bindings = []store.BoundProvenanceEventInput{bound}
	_, _, err := DeriveDocumentEvents(input)
	require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)

	input.Bindings[0].OriginalMTime = new("invalid")
	_, _, err = DeriveDocumentEvents(input)
	require.Error(t, err)
	require.NotErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
}

func TestAdaptersRawEmailSuccessOwnsInvalidNormalizedError(t *testing.T) {
	from := "Ada Example <ada@example.test>"
	input := documentEventInput("message/rfc822", []document.SourceMetadataFieldV1{
		stringField("email.from", "From", false, from),
		timestampField("email.sent", "email", "Date", false,
			"not-a-timestamp", "Tue, 2 Jan 2024 03:04 -0000", "second", "utc"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.Empty(t, record.Diagnostics)

	sent := requireEventKind(t, record.Events, "sent")
	require.Equal(t, "2024-01-02T03:04", sent.DateValue)
	require.Len(t, sent.Actors, 1)
	require.Equal(t, document.EventRole("sender"), sent.Actors[0].Role)
	require.Equal(t, "email:ada@example.test", sent.Actors[0].ActorKey)
	require.Empty(t, requireEventKind(t, record.Events, "vault_recorded").Actors)
}

func TestAdaptersRecoverCanonicalLegacyRawEmailClaims(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		normalized string
		dateValue  string
		precision  document.EventPrecision
		zone       document.EventTimezoneKind
		zoneText   string
	}{
		{
			name: "two digit year minute unknown offset",
			raw:  "Tue, 2 Jan 24 03:04 -0000", dateValue: "2024-01-02T03:04",
			normalized: "2024-01-02T03:04:00Z",
			precision:  "minute", zone: "omitted", zoneText: "-0000",
		},
		{
			name: "two digit year minute named zone",
			raw:  "Tue, 2 Jan 24 03:04 PST", dateValue: "2024-01-02T03:04",
			normalized: "2024-01-02T11:04:00Z",
			precision:  "minute", zone: "named", zoneText: "PST",
		},
		{
			name: "four digit year seconds named zone",
			raw:  "Tue, 2 Jan 2024 03:04:05 XYZ", dateValue: "2024-01-02T03:04:05",
			normalized: "2024-01-02T03:04:05Z",
			precision:  "second", zone: "named", zoneText: "XYZ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
				timestampField("email.sent", "email", "Date", false,
					test.normalized, test.raw, "second", "utc"),
			})
			record, _, err := DeriveDocumentEvents(input)
			require.NoError(t, err)
			sent := requireEventKind(t, record.Events, "sent")
			require.Equal(t, test.dateValue, sent.DateValue)
			require.Equal(t, test.precision, sent.Precision)
			require.Equal(t, test.zone, sent.TimezoneKind)
			require.Equal(t, test.zoneText, sent.ZoneText)
		})
	}
}

func TestAdaptersRejectUnsupportedRawEmailWithoutNormalizedFallback(t *testing.T) {
	input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
		timestampField("email.sent", "email", "Date", false,
			"2024-01-02T03:04:05Z", "unsupported raw date", "second", "utc"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.Len(t, record.Events, 1)
	require.Equal(t, document.DateKind("vault_recorded"), record.Events[0].DateKind)
	require.Len(t, record.Diagnostics, 1)
	require.Equal(t, "date_unparseable", record.Diagnostics[0].Code)
}

func TestAdaptersIgnoreLongNonActorEmailMetadataForActorBounds(t *testing.T) {
	subject := strings.Repeat("s", document.MaxDocumentEventActorClaimBytes+1)
	input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
		stringField("email.subject", "Subject", false, subject),
		timestampField("email.sent", "email", "Date", false,
			"2024-01-02T03:04:00Z", "Tue, 2 Jan 2024 03:04 -0000", "second", "utc"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.Equal(t, "2024-01-02T03:04", requireEventKind(t, record.Events, "sent").DateValue)
}

func TestAdaptersEnforceCombinedEventAndActorBoundsBeforeAppending(t *testing.T) {
	t.Run("event boundary", func(t *testing.T) {
		input := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{
			timestampField("created", "pdf.info", "CreationDate", false,
				"2024-01-02", "D:20240102", "date", "omitted"),
		})
		input.Bindings = make([]store.BoundProvenanceEventInput, document.MaxDocumentEvents-2)
		for index := range input.Bindings {
			input.Bindings[index] = boundDocumentEventInput(input.Target.ContentVersionID, index)
		}
		record, _, err := DeriveDocumentEvents(input)
		require.NoError(t, err)
		require.Len(t, record.Events, document.MaxDocumentEvents)
	})

	t.Run("event one over", func(t *testing.T) {
		input := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{
			timestampField("created", "pdf.info", "CreationDate", false,
				"2024-01-02", "D:20240102", "date", "omitted"),
		})
		input.Bindings = make([]store.BoundProvenanceEventInput, document.MaxDocumentEvents-1)
		for index := range input.Bindings {
			input.Bindings[index] = boundDocumentEventInput(input.Target.ContentVersionID, index)
		}
		_, _, err := DeriveDocumentEvents(input)
		require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
	})

	t.Run("claim byte one over", func(t *testing.T) {
		boundary := "a@example.test" + strings.Repeat(" ",
			document.MaxDocumentEventActorClaimBytes-len("a@example.test"))
		input := documentEventInput("message/rfc822", []document.SourceMetadataFieldV1{
			stringField("email.to", "To", false, boundary),
		})
		_, _, err := DeriveDocumentEvents(input)
		require.NoError(t, err)

		claim := strings.Repeat("x", document.MaxDocumentEventActorClaimBytes+1)
		input = documentEventInput("message/rfc822", []document.SourceMetadataFieldV1{
			stringField("email.to", "To", false, claim),
		})
		_, _, err = DeriveDocumentEvents(input)
		require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
	})

	t.Run("actor boundary and one over", func(t *testing.T) {
		for _, actorCount := range []int{document.MaxDocumentEventActors, document.MaxDocumentEventActors + 1} {
			authors := make([]string, 0, actorCount)
			for index := range actorCount {
				authors = append(authors, fmt.Sprintf("author-%04d", index))
			}
			fields := []document.SourceMetadataFieldV1{{
				Key: "creators", Namespace: "test", SourceField: "Creator",
				Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataStringList, Strings: authors},
			}}
			_, _, err := DeriveDocumentEvents(documentEventInput("application/pdf", fields))
			if actorCount == document.MaxDocumentEventActors {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
			}
		}
	})
}

func TestAdaptersClassifyAggregateTextBoundAsUnavailable(t *testing.T) {
	header := strings.TrimSuffix(strings.Repeat("a@b,", 900), ",")
	input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
		stringField("email.to", "To", false, header),
		stringField("email.cc", "Cc", false, header),
		stringField("email.bcc", "Bcc", false, header),
	})

	_, _, err := DeriveDocumentEvents(input)
	require.ErrorContains(t, err, "document event text is longer")
	require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
}

func TestAdaptersClassifyFinalCanonicalByteBoundAsUnavailable(t *testing.T) {
	display := strings.Repeat(`\\`, 1000)
	header := `"` + display + `" <a@b>,` + strings.TrimSuffix(strings.Repeat("a@b,", 395), ",")
	require.LessOrEqual(t, len(header), document.MaxDocumentEventActorClaimBytes)
	input := canonicalDocumentEventInput(t, []document.SourceMetadataFieldV1{
		stringField("email.from", "From", false, header),
		stringField("email.to", "To", false, header),
		stringField("email.cc", "Cc", false, header),
		stringField("email.bcc", "Bcc", false, header),
	})

	_, _, err := DeriveDocumentEvents(input)
	require.ErrorContains(t, err, "document events are longer")
	require.ErrorIs(t, err, store.ErrDocumentEventEvidenceUnavailable)
}

func TestAdaptersDoNotDiscloseSensitiveTimestampFailuresInDiagnostics(t *testing.T) {
	input := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{
		timestampField("created", "pdf.info", "CreationDate", false,
			"2020-01-02", "D:20200102", "date", "omitted"),
		timestampField("image.xmp.mystery", "image.xmp", "MysteryDate", false,
			"2020-01-03", "2020-01-03", "date", "omitted"),
		timestampField("modified", "pdf.info", "HiddenDate", true,
			"not-a-date", "private raw date", "date", "omitted"),
	})
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	require.NotEmpty(t, requireEventKind(t, record.Events, "created").EventID,
		"an invalid field must not delete known claims")
	require.Len(t, record.Diagnostics, 1)
	require.Equal(t, "date_kind_unknown", record.Diagnostics[0].Code)
	require.NotContains(t, record.Diagnostics[0].SourceKey, "modified")
	require.NotContains(t, record.Diagnostics[0].Detail, "private")
}

func TestAdaptersRejectMalformedOffsetWithoutInventingAnInstant(t *testing.T) {
	_, err := metadataTimestampEvent(document.SourceMetadataTimestampV1{
		Normalized: "2024-01-02T03:04:05+2:00", Offset: "+2:00",
		Precision: document.SourceMetadataPrecisionSecond,
		Raw:       "2024-01-02 03:04:05 +2:00", Timezone: document.SourceMetadataTimezoneOffset,
	})
	require.ErrorContains(t, err, "offset")
}

func TestAdaptersPreferEmbeddedModifiedClaimOverObservedFilesystemMTime(t *testing.T) {
	mtime := "2024-01-02T03:04:05.123456789Z"
	input := documentEventInput("application/pdf", []document.SourceMetadataFieldV1{
		timestampField("modified", "pdf.info", "ModDate", false,
			"2023-06-07", "D:20230607", "date", "omitted"),
	})
	input.Bindings = []store.BoundProvenanceEventInput{{
		Binding: store.ProvenanceVersionBinding{
			ProvenanceIdentity: testDigest("provenance"),
			ContentVersionID:   input.Target.ContentVersionID,
			ObservedAt:         "2024-01-02T03:04:06Z",
			BasisRef:           "ingest:exact-version",
		},
		OriginalMTime: &mtime, IngestStartedAt: "2024-01-02T03:04:04Z",
		EvidenceSHA256: testDigest("binding"),
	}}
	record, _, err := DeriveDocumentEvents(input)
	require.NoError(t, err)
	var sourceModified, observedModified document.DocumentEventV1
	for _, event := range record.Events {
		if event.DateKind != "modified" {
			continue
		}
		if event.EvidenceKind == "source_metadata" {
			sourceModified = event
		} else {
			observedModified = event
		}
	}
	require.Equal(t, "source_asserted", sourceModified.ClaimBasis)
	require.Equal(t, "docbank_observed", observedModified.ClaimBasis)
	primary := requirePrimary(t, record.Primaries, "full")
	require.Equal(t, sourceModified.EventID, primary.EventID)
}

func documentEventInput(mimeType string, fields []document.SourceMetadataFieldV1) store.DocumentEventEvidenceSnapshot {
	return store.DocumentEventEvidenceSnapshot{
		VaultUID: "00000000-0000-4000-8000-000000000001",
		Target: store.DocumentEventTarget{
			ContentVersionID: "00000000-0000-4000-8000-000000000002",
			NodeID:           7, BlobHash: testDigest("blob"), Size: 42, MIMEType: mimeType,
			RecordedAt: "2025-02-03T04:05:06.123456789Z", InputEpoch: 3, InputRevision: 4,
		},
		MetadataGeneration: store.SourceMetadataGeneration{
			GenerationID: "meta-generation", SourceSHA256: testDigest("blob"),
			ContractVersion:      document.SourceMetadataContractV1,
			ExtractorFingerprint: testDigest("extractor"), Checksum: testDigest("metadata"),
		},
		Metadata: document.SourceMetadataV1{
			ContractVersion: document.SourceMetadataContractV1,
			Fields:          fields, Warnings: []document.SourceMetadataWarningV1{},
		},
		Bindings: []store.BoundProvenanceEventInput{}, InputsSHA256: testDigest("inputs"),
	}
}

func canonicalDocumentEventInput(
	t *testing.T,
	fields []document.SourceMetadataFieldV1,
) store.DocumentEventEvidenceSnapshot {
	t.Helper()
	input := documentEventInput("message/rfc822", fields)
	canonical, checksum, err := document.MarshalSourceMetadataV1(input.Metadata)
	require.NoError(t, err)
	decoded, decodedChecksum, err := document.DecodeSourceMetadataV1(canonical)
	require.NoError(t, err)
	require.Equal(t, checksum, decodedChecksum)
	input.Metadata = decoded
	input.MetadataGeneration.Checksum = checksum
	input.MetadataGeneration.CanonicalJSON = canonical
	return input
}

func boundDocumentEventInput(versionID string, index int) store.BoundProvenanceEventInput {
	observed := "2024-01-02T03:04:05Z"
	return store.BoundProvenanceEventInput{
		Binding: store.ProvenanceVersionBinding{
			ProvenanceIdentity: testDigest(fmt.Sprintf("provenance-%d", index)),
			ContentVersionID:   versionID, ObservedAt: observed, BasisRef: "test",
		},
		IngestStartedAt: observed, EvidenceSHA256: testDigest(fmt.Sprintf("binding-%d", index)),
	}
}

func timestampField(key, namespace, sourceField string, sensitive bool,
	normalized, raw string,
	precision document.SourceMetadataTimestampPrecision,
	timezone document.SourceMetadataTimezoneKind,
) document.SourceMetadataFieldV1 {
	return document.SourceMetadataFieldV1{
		Key: key, Namespace: namespace, SourceField: sourceField, Sensitive: sensitive,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp,
			Timestamp: &document.SourceMetadataTimestampV1{
				Normalized: normalized, Raw: raw,
				Precision: precision, Timezone: timezone,
			}},
	}
}

func stringField(key, sourceField string, sensitive bool, value string) document.SourceMetadataFieldV1 {
	return document.SourceMetadataFieldV1{
		Key: key, Namespace: "email", SourceField: sourceField, Sensitive: sensitive,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &value},
	}
}

func requireEventKind(t *testing.T, events []document.DocumentEventV1, kind document.DateKind) document.DocumentEventV1 {
	t.Helper()
	for _, event := range events {
		if event.DateKind == kind {
			return event
		}
	}
	t.Fatalf("event kind %q was not derived", kind)
	return document.DocumentEventV1{}
}

func requirePrimary(t *testing.T, primaries []document.DocumentEventPrimaryV1, disclosure string) document.DocumentEventPrimaryV1 {
	t.Helper()
	for _, primary := range primaries {
		if primary.ScopeClass == "vault" && primary.Disclosure == disclosure {
			return primary
		}
	}
	t.Fatalf("vault primary %s was not selected", disclosure)
	return document.DocumentEventPrimaryV1{}
}

func testDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}
