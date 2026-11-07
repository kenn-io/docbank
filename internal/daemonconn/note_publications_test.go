package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestNotePublicationRoundTrip(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, serverKey)
	scan, err := c.BeginNotePublicationScan(t.Context(), api.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	page, err := c.NotePublicationSources(t.Context(), api.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, scan, page.Scan)
	receipt, err := c.UploadPublishedNote(t.Context(), api.NotePublicationUploadRequest{Name: "research", ScanID: scan.ScanID, SourceRef: "a.md", FileName: "a.md", ParentPath: "/notes", OperationID: uuid.NewV4().String(), Hash: "edb465624291e4053c6c5ea4b7eb320dec773e10a57d26b95dcf0564f8e310f8", Size: 4}, strings.NewReader("note"))
	require.NoError(t, err)
	require.Equal(t, "added", receipt.Outcome)
	ack, err := c.AcknowledgePublishedNotes(t.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: scan.ScanID, Bindings: []api.PublishedNoteIdentity{receipt.Source.Identity}})
	require.NoError(t, err)
	require.Equal(t, receipt.Source.Identity, ack.Sources[0])
	status, err := c.CompleteNotePublicationScan(t.Context(), api.NotePublicationCompleteRequest{Name: "research", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Present)
	require.False(t, status.Incomplete)
	read, err := c.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.Equal(t, status, read)
}

func TestNotePublicationMalformedSuccess(t *testing.T) {
	t.Parallel()
	scan := api.NotePublicationScan{ScanID: uuid.NewV4().String(), DatabaseEpoch: uuid.NewV4().String(), PolicyFingerprint: strings.Repeat("a", 64), Epoch: 1, NamespaceRevision: 1}
	for _, test := range []struct {
		name string
		body any
		call func(*daemonconn.Connection) error
	}{
		{"begin", api.NotePublicationScan{}, func(c *daemonconn.Connection) error {
			_, err := c.BeginNotePublicationScan(t.Context(), api.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: scan.PolicyFingerprint, OperationID: uuid.NewV4().String()})
			return err
		}},
		{"empty continuation", api.NotePublicationSourcesPage{Scan: scan, Next: "a.md"}, func(c *daemonconn.Connection) error {
			_, err := c.NotePublicationSources(t.Context(), api.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
			return err
		}},
		{"status", api.NotePublicationStatus{Name: "other"}, func(c *daemonconn.Connection) error {
			_, err := c.NotePublicationStatus(t.Context(), "research")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				b, _ := json.Marshal(test.body)
				_, _ = w.Write(b)
			}))
			defer ts.Close()
			require.True(t, daemonconn.IsResponseDecodeError(test.call(daemonconn.New(ts.URL, serverKey))))
		})
	}
}

func TestNotePublicationAcknowledgementRejectsWrongBindings(t *testing.T) {
	t.Parallel()
	scan := api.NotePublicationScan{ScanID: uuid.NewV4().String(), DatabaseEpoch: uuid.NewV4().String(), PolicyFingerprint: strings.Repeat("a", 64), Epoch: 1, NamespaceRevision: 1}
	id := api.PublishedNoteIdentity{VaultID: uuid.NewV4().String(), Publication: "research", SourceRef: "a.md", NodeID: 1, ContentVersionID: uuid.NewV4().String(), Hash: strings.Repeat("a", 64), Size: 5, SourceRevision: 1, NamespaceRevision: 1}
	other := id
	other.SourceRef = "b.md"
	other.NodeID = 2
	other.ContentVersionID = uuid.NewV4().String()
	for _, kind := range []string{"order", "missing", "extra", "version", "hash", "revision", "namespace"} {
		t.Run(kind, func(t *testing.T) {
			ack := api.NotePublicationAck{Scan: scan, Sources: []api.PublishedNoteIdentity{id, other}}
			switch kind {
			case "order":
				ack.Sources[0], ack.Sources[1] = ack.Sources[1], ack.Sources[0]
			case "missing":
				ack.Sources = ack.Sources[:1]
			case "extra":
				ack.Sources = append(ack.Sources, id)
			case "version":
				ack.Sources[0].ContentVersionID = other.ContentVersionID
			case "hash":
				ack.Sources[0].Hash = strings.Repeat("b", 64)
			case "revision":
				ack.Sources[0].SourceRevision += 2
			case "namespace":
				ack.Sources[0].NamespaceRevision = 0
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				b, _ := json.Marshal(ack)
				_, _ = w.Write(b)
			}))
			defer ts.Close()
			_, err := daemonconn.New(ts.URL, serverKey).AcknowledgePublishedNotes(t.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: scan.ScanID, Bindings: []api.PublishedNoteIdentity{id, other}})
			require.True(t, daemonconn.IsResponseDecodeError(err))
		})
	}
}

func TestNotePublicationRejectsOversizedSuccess(t *testing.T) {
	t.Parallel()
	status := api.NotePublicationStatus{Name: "research", DatabaseEpoch: uuid.NewV4().String()}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat(" ", 128<<10)))
		b, _ := json.Marshal(status)
		_, _ = w.Write(b)
	}))
	defer ts.Close()
	_, err := daemonconn.New(ts.URL, serverKey).NotePublicationStatus(t.Context(), "research")
	require.True(t, daemonconn.IsResponseDecodeError(err))
}

func TestNotePublicationInterruptedReceiptStaysRetryable(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "200")
		_, _ = w.Write([]byte(`{"name":"research"`))
	}))
	defer ts.Close()
	_, err := daemonconn.New(ts.URL, serverKey).NotePublicationStatus(t.Context(), "research")
	require.ErrorIs(t, err, daemonconn.ErrPushResponseInterrupted)
	require.False(t, daemonconn.IsResponseDecodeError(err))
}

func TestNotePublicationCursorRejectsUnboundRecovery(t *testing.T) {
	t.Parallel()
	scan := api.NotePublicationScan{ScanID: uuid.NewV4().String(), DatabaseEpoch: uuid.NewV4().String(), PolicyFingerprint: strings.Repeat("a", 64), Epoch: 1, NamespaceRevision: 1}
	id := api.PublishedNoteIdentity{VaultID: uuid.NewV4().String(), Publication: "research", SourceRef: "a.md", NodeID: 1, ContentVersionID: uuid.NewV4().String(), Hash: strings.Repeat("a", 64), Size: 5, SourceRevision: 1, NamespaceRevision: 1}
	head := uuid.NewV4().String()
	for _, kind := range []string{"head", "source", "revision", "unmarked divergence", "unavailable grant", "missing head"} {
		t.Run(kind, func(t *testing.T) {
			source := api.NotePublicationSource{Identity: id, Presence: "present", Availability: "retained", Conflict: "diverged", ArchiveHead: &api.NoteArchiveHead{NodeID: 1, ContentVersionID: head, Hash: strings.Repeat("b", 64), Size: 6}, Recovery: &api.NotePublicationRecoveryGrant{SourceRef: "a.md", ExpectedHeadVersionID: head, SourceRevision: 1}}
			switch kind {
			case "head":
				source.Recovery.ExpectedHeadVersionID = id.ContentVersionID
			case "source":
				source.Recovery.SourceRef = "other.md"
			case "revision":
				source.Recovery.SourceRevision++
			case "unmarked divergence":
				source.Conflict = ""
				source.Recovery = nil
			case "unavailable grant":
				source.Availability = "unavailable"
			case "missing head":
				source.ArchiveHead = nil
				source.Recovery = nil
				source.Conflict = ""
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				b, _ := json.Marshal(api.NotePublicationSourcesPage{Scan: scan, Items: []api.NotePublicationSource{source}})
				_, _ = w.Write(b)
			}))
			defer ts.Close()
			_, err := daemonconn.New(ts.URL, serverKey).NotePublicationSources(t.Context(), api.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
			require.True(t, daemonconn.IsResponseDecodeError(err))
		})
	}
}
