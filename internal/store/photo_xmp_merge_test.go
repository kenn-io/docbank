package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The same ledger owns scalar values and the whole keyword set.
func TestPhotoXMPPropertyLedger(t *testing.T) {
	t.Parallel()
	packet := func(rating int, caption string, keywords ...string) PhotoXMPPacket {
		return PhotoXMPPacket{Rating: rating, Caption: caption, Present: PhotoConfirmedRating | PhotoConfirmedCaption | PhotoXMPKeywordsBit, Keywords: PhotoXMPKeywords(keywords)}
	}
	type catalogState struct {
		PhotoAuthored

		Keywords []string
	}
	decision := func(rating int, caption string, keywords ...string) catalogState {
		return catalogState{Rating: rating, Caption: caption, Confirmed: PhotoConfirmedRating | PhotoConfirmedCaption, Keywords: PhotoXMPKeywords(keywords)}
	}
	confirmedDefault := catalogState{Confirmed: PhotoConfirmedRating, Keywords: []string{}}
	for _, tc := range []struct {
		name                      string
		base, incoming            PhotoXMPPacket
		comparison, catalog       catalogState
		pending                   PhotoAuthoredFields
		choice                    string
		first                     bool
		want                      PhotoXMPPacket
		imports, write, conflicts PhotoAuthoredFields
		ancestor                  PhotoXMPPacket
	}{
		{name: "first missing values preserve catalog", catalog: decision(4, "Catalog"), first: true, want: packet(4, "Catalog"), write: PhotoConfirmedRating | PhotoConfirmedCaption, ancestor: packet(0, "")},
		{name: "first readable values import", incoming: packet(4, "Outside"), first: true, want: packet(4, "Outside"), imports: PhotoConfirmedRating | PhotoConfirmedCaption, ancestor: packet(4, "Outside")},
		{name: "existing sidecar receives confirmed default", catalog: confirmedDefault, first: true, want: packet(0, ""), write: PhotoConfirmedRating, ancestor: packet(0, "")},
		{name: "first confirmed default conflicts", catalog: confirmedDefault, incoming: packet(3, ""), first: true, want: packet(0, ""), write: PhotoConfirmedRating, conflicts: PhotoConfirmedRating, ancestor: packet(0, "")},
		{name: "converged value advances comparison", base: packet(3, ""), comparison: decision(3, ""), catalog: decision(4, ""), incoming: packet(4, ""), want: packet(4, ""), ancestor: packet(4, "")},
		{name: "one outside edit after convergence imports", base: packet(4, ""), comparison: decision(4, ""), catalog: decision(4, ""), incoming: packet(2, ""), want: packet(2, ""), imports: PhotoConfirmedRating, ancestor: packet(2, "")},
		{name: "divergent property holds packet", base: packet(3, "A"), comparison: decision(3, "A"), catalog: decision(5, "A"), incoming: packet(4, "B"), want: packet(5, "B"), imports: PhotoConfirmedCaption, write: PhotoConfirmedRating, conflicts: PhotoConfirmedRating, ancestor: packet(3, "B")},
		{name: "rejected outside value stays rejected", base: packet(4, "A"), comparison: decision(5, "A"), catalog: decision(5, "A"), incoming: packet(4, "B"), want: packet(5, "B"), imports: PhotoConfirmedCaption, write: PhotoConfirmedRating, ancestor: packet(5, "B")},
		{name: "sticky conflict survives catalog return", base: packet(3, ""), comparison: decision(3, ""), catalog: decision(3, ""), incoming: packet(1, ""), pending: PhotoConfirmedRating, want: packet(3, ""), write: PhotoConfirmedRating, conflicts: PhotoConfirmedRating, ancestor: packet(3, "")},
		{name: "catalog choice only advances divergent field", base: packet(5, "A"), comparison: decision(5, "A"), catalog: decision(3, "C"), incoming: packet(5, ""), pending: PhotoConfirmedCaption, choice: "catalog", want: packet(3, "C"), write: PhotoConfirmedRating | PhotoConfirmedCaption, ancestor: packet(5, "C")},
		{name: "sidecar choice retains unrelated comparison", base: packet(5, "A"), comparison: decision(5, "A"), catalog: decision(3, "C"), incoming: packet(5, ""), pending: PhotoConfirmedCaption, choice: "sidecar", want: packet(3, ""), imports: PhotoConfirmedCaption, write: PhotoConfirmedRating, ancestor: packet(5, "")},
		{name: "first divergent keywords conflict", catalog: decision(0, "", "Travel"), incoming: packet(0, "", "Harbor"), first: true, want: packet(0, "", "Travel"), write: PhotoXMPKeywordsBit, conflicts: PhotoXMPKeywordsBit, ancestor: packet(0, "")},
		{name: "catalog keyword choice replaces the set", base: packet(0, "", "Travel"), comparison: decision(0, "", "Travel"), catalog: decision(0, "", "River"), incoming: packet(0, "", "Travel"), pending: PhotoXMPKeywordsBit, choice: "catalog", want: packet(0, "", "River"), write: PhotoXMPKeywordsBit, ancestor: packet(0, "", "River")},
		{name: "outside keyword choice replaces the set", base: packet(0, "", "Travel"), comparison: decision(0, "", "Travel"), catalog: decision(0, "", "River"), incoming: packet(0, "", "Travel"), pending: PhotoXMPKeywordsBit, choice: "sidecar", want: packet(0, "", "Travel"), imports: PhotoXMPKeywordsBit, ancestor: packet(0, "", "Travel")},
		{name: "keyword conflict retains catalog set", base: packet(0, "", "Travel"), comparison: decision(0, "", "Travel"), catalog: decision(0, "", "River"), incoming: packet(0, "", "Travel"), pending: PhotoXMPKeywordsBit, want: packet(0, "", "River"), write: PhotoXMPKeywordsBit, conflicts: PhotoXMPKeywordsBit, ancestor: packet(0, "", "Travel")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ValidatePhotoAuthored(tc.catalog.PhotoAuthored))
			require.NoError(t, ValidatePhotoAuthored(tc.comparison.PhotoAuthored))
			work := PhotoXMPWork{Original: PhotoFile{Rating: tc.catalog.Rating, Caption: tc.catalog.Caption, Confirmed: tc.catalog.Confirmed}, Keywords: tc.catalog.Keywords, Conflicts: tc.pending, ConflictPath: "camera", Sidecar: &PhotoXMPSidecar{FileID: "40000000-0000-4000-8000-000000000003", Node: Node{ID: 3, CurrentVersionID: "40000000-0000-4000-8000-000000000004"}}}
			head := PhotoXMPHead{Path: "camera", Digest: fakeHash("d4"), VersionID: work.Sidecar.Node.CurrentVersionID, Size: 100}
			if !tc.first {
				work.History = &photoXMPSync{Outside: []PhotoXMPSource{{Path: "camera", Packet: tc.base, Catalog: tc.comparison.PhotoAuthored, Keywords: tc.comparison.Keywords}}}
				work.History.Outside[0].Digest = fakeHash("c3")
				work.History.Outside[0].VersionID = "40000000-0000-4000-8000-000000000005"
				work.History.Outside[0].SourceOrientation = 1
				work.History.Settled = work.History.Outside[0].PhotoXMPSnapshot
				work.History.Settled.Digest = fakeHash("e5")
				work.History.Settled.VersionID = "40000000-0000-4000-8000-000000000006"
				work.History.Settled.Packet.PhotoAuthored = tc.comparison.PhotoAuthored
				work.History.Settled.Packet.Confirmed = 0
				work.History.Settled.Packet.Keywords = tc.comparison.Keywords
				work.History.Sidecar = PhotoSidecarProvenance{NodeID: 3, FileID: work.Sidecar.FileID, VersionID: work.History.Settled.VersionID}
				head.Base = &work.History.Outside[0].PhotoXMPSnapshot
			}
			work.Heads = []PhotoXMPHead{head}
			steps := work.Steps()
			steps[0].Packet = tc.incoming
			result, write := PlanPhotoXMP(work, steps, tc.choice)
			require.NoError(t, ValidatePhotoAuthored(result.Values))
			assert.Equal(t, tc.want.Rating, result.Values.Rating)
			assert.Equal(t, tc.want.Caption, result.Values.Caption)
			assert.Equal(t, tc.want.Keywords, result.Keywords)
			assert.Equal(t, tc.imports != 0, result.Import)
			assert.Equal(t, tc.write, write)
			assert.Equal(t, tc.conflicts, result.Conflicts)
			require.Len(t, result.Outside, 1)
			require.NoError(t, ValidatePhotoAuthored(result.Outside[0].Catalog))
			assert.Equal(t, tc.ancestor.Rating, result.Outside[0].Catalog.Rating)
			assert.Equal(t, tc.ancestor.Caption, result.Outside[0].Catalog.Caption)
			assert.Equal(t, tc.ancestor.Keywords, result.Outside[0].Keywords)
		})
	}
}
