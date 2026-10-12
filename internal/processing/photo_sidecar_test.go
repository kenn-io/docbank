package processing

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

const photoSidecarHeader = `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:ts="https://teststrip.app/xmp/1.0/"`
const photoSidecarFooter = `</rdf:Description></rdf:RDF></x:xmpmeta>`

func TestReadPhotoSidecar(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("x", store.MaxPhotoAuthoredTextBytes+1)
	bounded := strings.Repeat("x", store.MaxPhotoAuthoredTextBytes)
	cases := []struct {
		name, packet string
		want         store.PhotoAuthored
		invalid      bool
	}{
		{name: "repeated values", packet: photoSidecarHeader + `><xmp:Rating><rdf:value>0</rdf:value><rdf:value>5</rdf:value></xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text before value", packet: photoSidecarHeader + `><xmp:Rating>0<rdf:value>5</rdf:value></xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "oversized text before value", packet: photoSidecarHeader + `><xmp:Rating>` + oversized + `<rdf:value>5</rdf:value></xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text after value", packet: photoSidecarHeader + `><xmp:Rating><rdf:value>5</rdf:value>0</xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text inside Alt", packet: photoSidecarHeader + `><dc:description><rdf:Alt>Caption<rdf:li>Other</rdf:li></rdf:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "padded value", packet: photoSidecarHeader + "><xmp:Rating>\n <rdf:value>5</rdf:value>\n </xmp:Rating>" + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5}},
		{name: "resource wrapper", packet: photoSidecarHeader + `><dc:creator rdf:parseType="Resource"><rdf:value>Author</rdf:value></dc:creator>` + photoSidecarFooter, want: store.PhotoAuthored{Creator: "Author"}},
		{name: "empty", packet: photoSidecarHeader + `>` + photoSidecarFooter},
		{name: "numeric zero", packet: photoSidecarHeader + ` ts:Rotation="0" xmp:Rating="0">` + photoSidecarFooter, want: store.PhotoAuthored{}},
		{name: "empty containers", packet: photoSidecarHeader + "><dc:description>\n <rdf:Alt>\n </rdf:Alt>\n</dc:description><dc:creator>\n <rdf:Seq>\n </rdf:Seq>\n</dc:creator><dc:rights><rdf:Alt/></dc:rights>" + photoSidecarFooter},
		{name: "blank scalar", packet: photoSidecarHeader + "><dc:description> \n\t </dc:description><dc:creator> </dc:creator><dc:rights>\n</dc:rights>" + photoSidecarFooter},
		{name: "blank selected items", packet: photoSidecarHeader + `><dc:description><rdf:Alt><rdf:li>Other</rdf:li><rdf:li xml:lang="x-default"> </rdf:li></rdf:Alt></dc:description><dc:creator><rdf:Seq><rdf:li> </rdf:li><rdf:li>Other</rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li> </rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter},
		{name: "blank attributes", packet: photoSidecarHeader + ` dc:description=" &#10; " dc:creator=" " dc:rights="&#9;">` + photoSidecarFooter},
		{name: "padded attributes", packet: photoSidecarHeader + ` dc:description="&#10; River &#10;" dc:creator=" Creator " dc:rights=" Copyright ">` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "\n River \n", Creator: " Creator ", Copyright: " Copyright "}},
		{name: "duplicate blank property", packet: photoSidecarHeader + ` dc:description=" "><dc:description>Caption</dc:description>` + photoSidecarFooter, invalid: true},
		{name: "attributes", packet: photoSidecarHeader + ` xmp:Rating="5" xmp:Label="Red" ts:Pick="pick" ts:Rotation="90">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5, Label: "red", Flag: "pick", Rotation: 90}},
		{name: "rejection sentinel", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="pick">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}},
		{name: "namespace aliases", packet: `<x:xmpmeta xmlns:x="adobe:ns:meta/"><r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:a="http://ns.adobe.com/xap/1.0/"><r:Description a:Rating="3"/></r:RDF></x:xmpmeta>`, want: store.PhotoAuthored{Rating: 3}},
		{name: "wrong namespace", packet: photoSidecarHeader + ` xmlns:bad="https://example.org/xmp" bad:Rating="5" bad:Pick="pick">` + photoSidecarFooter},
		{name: "rdf text", packet: photoSidecarHeader + `><xmp:Rating>4</xmp:Rating><dc:description><rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="x-default">River &amp; sky</rdf:li></rdf:Alt></dc:description><dc:creator><rdf:Seq><rdf:li>Creator A</rdf:li><rdf:li>Creator B</rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li>Copyright example</rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River & sky", Creator: "Creator A", Copyright: "Copyright example"}},
		{name: "custom label", packet: photoSidecarHeader + ` xmp:Rating="4" xmp:Label="Approved"><dc:description>Caption</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "Caption"}},
		{name: "oversized caption", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>` + oversized + `</dc:description><dc:creator>Creator</dc:creator><dc:rights>Copyright</dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Flag: "pick", Creator: "Creator", Copyright: "Copyright"}},
		{name: "oversized creator", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>River</dc:description><dc:creator>` + oversized + `</dc:creator><dc:rights>Copyright</dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Flag: "pick", Caption: "River", Copyright: "Copyright"}},
		{name: "oversized copyright", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>River</dc:description><dc:creator>Creator</dc:creator><dc:rights>` + oversized + `</dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Flag: "pick", Caption: "River", Creator: "Creator"}},
		{name: "maximum text bytes", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>` + bounded + `</dc:description><dc:creator>` + bounded + `</dc:creator><dc:rights>` + bounded + `</dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Flag: "pick", Caption: bounded, Creator: bounded, Copyright: bounded}},
		{name: "out-of-range rating", packet: photoSidecarHeader + ` xmp:Rating="6" xmp:Label="Red">` + photoSidecarFooter, want: store.PhotoAuthored{Label: "red"}},
		{name: "real rating", packet: photoSidecarHeader + ` xmp:Rating="3.0">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 3}},
		{name: "fractional rating", packet: photoSidecarHeader + ` xmp:Rating="2.5" xmp:Label="Red">` + photoSidecarFooter, want: store.PhotoAuthored{Label: "red"}},
		{name: "bad rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="45"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}},
		{name: "malformed rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="bad">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}},
		{name: "authored whitespace", packet: photoSidecarHeader + `><dc:description>` + "\n  River\n" + `</dc:description><dc:creator><rdf:Seq><rdf:li>  Creator  </rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li xml:lang="x-default"> Copyright </rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "\n  River\n", Creator: "  Creator  ", Copyright: " Copyright "}},
		{name: "unsupported pick", packet: photoSidecarHeader + ` xmp:Rating="2" ts:Pick="yes">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 2}},
		{name: "malformed suffix", packet: photoSidecarHeader + ` xmp:Rating="5">` + photoSidecarFooter + `<`, invalid: true},
		{name: "two roots", packet: strings.Repeat(photoSidecarHeader+`>`+photoSidecarFooter, 2), invalid: true},
		{name: "no RDF", packet: `<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`, invalid: true},
		{name: "wrong RDF value namespace", packet: photoSidecarHeader + `><dc:description><bad:Alt xmlns:bad="https://example.org/rdf"><bad:li>Hidden</bad:li></bad:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "rejection with unsupported pick", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="unknown">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}},
		{name: "duplicate", packet: photoSidecarHeader + ` xmp:Rating="5"><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "other subject", packet: photoSidecarHeader + ` rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="thumbnail" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}},
		{name: "same default subject", packet: photoSidecarHeader + ` rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}},
		{name: "nested subject", packet: photoSidecarHeader + ` xmp:Rating="4"><metadata><rdf:RDF><rdf:Description xmp:Rating="1"/></rdf:RDF></metadata>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}},
		{name: "identifier kinds differ", packet: photoSidecarHeader + ` rdf:nodeID="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="photo" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}},
		{name: "matching node ID", packet: photoSidecarHeader + ` rdf:nodeID="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:nodeID="photo" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}},
		{name: "ID with empty about", packet: photoSidecarHeader + ` rdf:ID="photo" rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description rdf:ID="photo" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}},
		{name: "same subject duplicate", packet: photoSidecarHeader + ` rdf:about="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="photo" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, invalid: true},
		{name: "deep", packet: photoSidecarHeader + `>` + strings.Repeat(`<a>`, 65) + strings.Repeat(`</a>`, 65) + photoSidecarFooter, invalid: true},
		{name: "padded rating split by comment", packet: photoSidecarHeader + `><xmp:Rating>3` + strings.Repeat(" ", store.MaxPhotoAuthoredTextBytes) + `<!--split-->invalid</xmp:Rating><xmp:Label>Red</xmp:Label>` + photoSidecarFooter, want: store.PhotoAuthored{Label: "red"}},
		{name: "oversized alternative beside default", packet: photoSidecarHeader + `><dc:description><rdf:Alt><rdf:li xml:lang="fr">` + oversized + `</rdf:li><rdf:li xml:lang="x-default">River</rdf:li></rdf:Alt></dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "River"}},
		{name: "large editing data", packet: photoSidecarHeader + ` xmp:Rating="5"><edit:Brush xmlns:edit="https://example.org/edit">` + strings.Repeat("x", 2<<20) + `</edit:Brush>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadPhotoSidecar(t.Context(), []byte(c.packet))
			if c.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, c.want, got)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ReadPhotoSidecar(ctx, []byte(photoSidecarHeader+`>`+photoSidecarFooter))
	require.ErrorIs(t, err, context.Canceled)
}

func photoSidecarFixture(t *testing.T, packet []byte) (*store.Store, *blob.Store, store.Node, store.Node) {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	create := func(name string, data []byte, mime string) store.Node {
		receipt, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(data))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		node, err := catalog.CreateFile(ctx, catalog.RootID(), name, receipt.Hash, receipt.Size, mime, store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created})
		require.NoError(t, err)
		return node
	}
	photo := create("photo.jpg", []byte("synthetic photo"), "image/jpeg")
	sidecar := create("photo.xmp", packet, "application/rdf+xml")
	asset, err := catalog.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	targetID := asset.Files[0].ID
	_, err = catalog.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, store.PhotoRoleSidecar, &targetID)
	require.NoError(t, err)
	return catalog, blobs, photo, sidecar
}

func TestPhotoSidecarSourceEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, packet string
		valid        bool
	}{
		{"prolog and packet", "\xef\xbb\xbf" + `<?xml version="1.0"?><?xpacket begin=""?>` + photoSidecarHeader + ` xmp:Rating="4">` + photoSidecarFooter + `<?xpacket end="w"?>`, true},
		{"invalid", photoSidecarHeader + ` xmp:Rating="4"><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			packet := []byte(test.packet)
			catalog, blobs, _, sidecar := photoSidecarFixture(t, packet)
			count, err := BackfillSourceMetadataTargets(ctx, catalog, blobs, sourceMetadataTestSpool(t), []store.SourceMetadataTarget{{SourceSHA256: sidecar.BlobHash, Size: sidecar.Size}})
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			view, err := catalog.ContentVersionSourceMetadata(ctx, sidecar.CurrentVersionID)
			require.NoError(t, err)
			valid := false
			for _, field := range view.Metadata.Fields {
				if field.Key == "image.xmp.packet_valid" {
					valid = field.Value.Boolean != nil && *field.Value.Boolean
				}
			}
			assert.Equal(t, test.valid, valid)
			if !test.valid {
				assert.NotEmpty(t, view.Metadata.Warnings)
			}
		})
	}
}

func TestPhotoSidecarImportInitialization(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy", "extension claim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			claim, targetRole := "", store.PhotoRoleRAW
			if mode == "extension claim" {
				claim, targetRole = ` xmlns:ps="http://ns.adobe.com/photoshop/1.0/" ps:SidecarForExtension="JPG"`, store.PhotoRoleImage
			}
			catalog, imported := importPhotoGroupWithMetadata(t, []photoGroupFile{
				{"capture.ARW", store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"},
				{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"},
				{"capture.XMP", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + claim + ` xmp:Rating="4" xmp:Label="Red"><dc:description>River</dc:description>` + photoSidecarFooter},
			})
			sidecars, err := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 1)
			require.NoError(t, err)
			require.Len(t, sidecars, 1)
			receipt, err := catalog.InitializePhotoSidecar(ctx, sidecars[0])
			require.NoError(t, err)
			require.Len(t, receipt.After, 1)
			require.NotNil(t, receipt.Sidecar)
			assert.Equal(t, sidecars[0].SidecarFileID, receipt.Sidecar.FileID)
			asset, err := catalog.PhotoAssetByID(ctx, imported.ID)
			require.NoError(t, err)
			for _, file := range asset.Files {
				if file.Role == targetRole {
					assert.Equal(t, store.PhotoAuthored{Rating: 4, Label: "red", Caption: "River", Confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedLabel | store.PhotoConfirmedCaption}, file.Authored())
					assert.Equal(t, int64(2), file.Revision)
					assert.Equal(t, file.ID, receipt.After[0].FileID)
				} else {
					assert.Equal(t, store.PhotoAuthored{}, file.Authored())
					assert.Equal(t, int64(1), file.Revision)
				}
			}
		})
	}
}

type photoGroupFile struct{ name, role, mime, payload string }

// importPhotoGroupWithMetadata imports one photo group and extracts source
// metadata for every member, as the daemon does before the sidecar lane runs.
func importPhotoGroupWithMetadata(t *testing.T, files []photoGroupFile) (*store.Store, store.PhotoAsset) {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	group := store.PhotoImportGroup{DestinationID: catalog.RootID()}
	for _, file := range files {
		receipt, err := blobs.WriteDetailedContext(ctx, strings.NewReader(file.payload))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		group.Members = append(group.Members, store.PhotoImportMember{Name: file.name, Role: file.role, BlobHash: receipt.Hash, Size: receipt.Size, MediaType: file.mime, OriginalPath: filepath.Join(root, "camera", file.name), Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created}})
	}
	run, err := catalog.BeginIngest(ctx, "photo-import", filepath.Join(root, "camera"))
	require.NoError(t, err)
	imported, err := catalog.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	pending, err := catalog.MissingSourceMetadataTargetsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
	require.NoError(t, err)
	_, err = BackfillSourceMetadataTargets(ctx, catalog, blobs, sourceMetadataTestSpool(t), pending)
	require.NoError(t, err)
	return catalog, imported.Asset
}

func TestPhotoSidecarImportAppliesEachQualifiedPacket(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	catalog, imported := importPhotoGroupWithMetadata(t, []photoGroupFile{
		{"capture.ARW", store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"},
		{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"},
		{"capture.ARW.xmp", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmp:Rating="5">` + photoSidecarFooter},
		{"capture.JPG.xmp", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmp:Rating="2">` + photoSidecarFooter},
	})
	for range 4 {
		pending, err := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
		require.NoError(t, err)
		if len(pending) == 0 {
			break
		}
		for _, target := range pending {
			_, err := catalog.InitializePhotoSidecar(ctx, target)
			require.NoError(t, err)
		}
	}
	pending, err := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	asset, err := catalog.PhotoAssetByID(ctx, imported.ID)
	require.NoError(t, err)
	ratings := map[string]int{}
	for _, file := range asset.Files {
		ratings[file.Role] = file.Rating
	}
	assert.Equal(t, map[string]int{store.PhotoRoleRAW: 5, store.PhotoRoleImage: 2, store.PhotoRoleSidecar: 0}, ratings)
}

func TestMetadataCollectorAuthoredTextPresence(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"image.xmp.caption", "image.xmp.creator", "image.xmp.copyright"} {
		collector := metadataCollector{record: new(emptySourceMetadata()), seen: map[string]bool{}}
		collector.string(key, "image.xmp", "text", " \n\t ", false)
		assert.Empty(t, collector.record.Fields)
		collector.string(key, "image.xmp", "text", " \n Meaningful \n ", false)
		require.Len(t, collector.record.Fields, 1)
		assert.Equal(t, " \n Meaningful \n ", *collector.record.Fields[0].Value.String)
	}
}
