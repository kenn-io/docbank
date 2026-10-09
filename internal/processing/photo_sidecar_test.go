package processing

import (
	"bytes"
	"context"
	"io"
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
	cases := []struct {
		name, packet string
		want         store.PhotoAuthored
		invalid      bool
	}{
		{name: "unrelated root", packet: `<foo><r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><r:Description/></r:RDF></foo>`, invalid: true},
		{name: "bare RDF root", packet: `<r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:a="http://ns.adobe.com/xap/1.0/"><r:Description a:Rating="3"/></r:RDF>`, invalid: true},
		{name: "empty", packet: photoSidecarHeader + `>` + photoSidecarFooter},
		{name: "attributes", packet: photoSidecarHeader + ` xmp:Rating="5" xmp:Label="Red" ts:Pick="pick" ts:Rotation="90">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5, Label: "red", Flag: "pick", Rotation: 90}},
		{name: "rejection sentinel", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="pick">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}},
		{name: "namespace aliases", packet: `<x:xmpmeta xmlns:x="adobe:ns:meta/"><r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:a="http://ns.adobe.com/xap/1.0/"><r:Description a:Rating="3"/></r:RDF></x:xmpmeta>`, want: store.PhotoAuthored{Rating: 3}},
		{name: "wrong namespace", packet: photoSidecarHeader + ` xmlns:bad="https://example.org/xmp" bad:Rating="5" bad:Pick="pick">` + photoSidecarFooter},
		{name: "rdf text", packet: photoSidecarHeader + `><xmp:Rating>4</xmp:Rating><dc:description><rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="x-default">River &amp; sky</rdf:li></rdf:Alt></dc:description><dc:creator><rdf:Seq><rdf:li>Creator A</rdf:li><rdf:li>Creator B</rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li>Copyright example</rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River & sky", Creator: "Creator A", Copyright: "Copyright example"}},
		{name: "custom label", packet: photoSidecarHeader + ` xmp:Rating="4" xmp:Label="Approved"><dc:description>Caption</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "Caption"}},
		{name: "bad rating", packet: photoSidecarHeader + ` xmp:Rating="6">` + photoSidecarFooter, invalid: true},
		{name: "bad rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="45"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}},
		{name: "malformed rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="bad">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}},
		{name: "authored whitespace", packet: photoSidecarHeader + `><dc:description>` + "\n  River\n" + `</dc:description><dc:creator><rdf:Seq><rdf:li>  Creator  </rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li xml:lang="x-default"> Copyright </rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "\n  River\n", Creator: "  Creator  ", Copyright: " Copyright "}},
		{name: "bad pick", packet: photoSidecarHeader + ` ts:Pick="yes">` + photoSidecarFooter, invalid: true},
		{name: "malformed suffix", packet: photoSidecarHeader + ` xmp:Rating="5">` + photoSidecarFooter + `<`, invalid: true},
		{name: "two roots", packet: photoSidecarHeader + `>` + photoSidecarFooter + `<empty/>`, invalid: true},
		{name: "no RDF", packet: `<empty/>`, invalid: true},
		{name: "wrong RDF value namespace", packet: photoSidecarHeader + `><dc:description><bad:Alt xmlns:bad="https://example.org/rdf"><bad:li>Hidden</bad:li></bad:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "rejection with invalid pick", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="unknown">` + photoSidecarFooter, invalid: true},
		{name: "duplicate", packet: photoSidecarHeader + ` xmp:Rating="5"><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "deep", packet: photoSidecarHeader + `>` + strings.Repeat(`<a>`, 65) + strings.Repeat(`</a>`, 65) + photoSidecarFooter, invalid: true},
		{name: "large", packet: strings.Repeat(" ", maxPhotoSidecarBytes+1), invalid: true},
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

func photoSidecarFixture(t *testing.T, packet []byte) (*store.Store, *blob.Store, store.Node, store.Node, store.PhotoSidecarTarget) {
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
	return catalog, blobs, photo, sidecar, store.PhotoSidecarTarget{FileID: targetID, AssetID: asset.ID, SidecarFileID: fileIDForNode(t, catalog, sidecar.ID), NodeID: sidecar.ID, VersionID: sidecar.CurrentVersionID, SourceSHA256: sidecar.BlobHash, Size: sidecar.Size, ExtractorFingerprint: SourceMetadataExtractorFingerprint}
}

func fileIDForNode(t *testing.T, catalog *store.Store, nodeID int64) string {
	t.Helper()
	asset, err := catalog.PhotoAssetForNode(t.Context(), nodeID)
	require.NoError(t, err)
	for _, f := range asset.Files {
		if f.NodeID == nodeID {
			return f.ID
		}
	}
	t.Fatal("missing sidecar member")
	return ""
}

func TestPhotoSidecarSourceEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, packet string
		rating       int
		valid        bool
	}{
		{"prolog and packet", "\xef\xbb\xbf" + `<?xml version="1.0"?><?xpacket begin=""?>` + photoSidecarHeader + ` xmp:Rating="4">` + photoSidecarFooter + `<?xpacket end="w"?>`, 4, true},
		{"empty", photoSidecarHeader + `>` + photoSidecarFooter, 0, true},
		{"invalid", photoSidecarHeader + ` xmp:Rating="7">` + photoSidecarFooter, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			packet := []byte(test.packet)
			catalog, blobs, photo, sidecar, target := photoSidecarFixture(t, packet)
			pending, err := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
			count, err := BackfillSourceMetadataTargets(ctx, catalog, blobs, t.TempDir(), []store.SourceMetadataTarget{{SourceSHA256: sidecar.BlobHash, Size: sidecar.Size}})
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
			var backup bytes.Buffer
			require.NoError(t, catalog.ExportMetadata(ctx, &backup))
			restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, restored.Close()) }()
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			next, err := restored.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
			require.NoError(t, err)
			if test.rating > 0 {
				require.Len(t, next, 1)
				receipt, applyErr := restored.InitializePhotoSidecar(ctx, next[0])
				require.NoError(t, applyErr)
				require.NotEmpty(t, receipt.ReceiptID)
				asset, assetErr := restored.PhotoAssetForNode(ctx, photo.ID)
				require.NoError(t, assetErr)
				for _, f := range asset.Files {
					if f.ID == target.FileID {
						assert.Equal(t, test.rating, f.Rating)
						assert.Empty(t, f.Label)
						assert.Equal(t, int64(2), f.Revision)
					}
				}
			} else {
				require.Len(t, next, 1)
				unchanged, nodeErr := restored.NodeByID(ctx, photo.ID)
				require.NoError(t, nodeErr)
				assert.Equal(t, photo, unchanged)
				_, applyErr := restored.InitializePhotoSidecar(ctx, target)
				require.NoError(t, applyErr)
				next, err = restored.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
				require.NoError(t, err)
				assert.Empty(t, next)
				receiptBytes := backup.String()
				assert.NotContains(t, receiptBytes, "sidecar_result")
				assert.NotContains(t, receiptBytes, "authored_sidecar")
				replacement := []byte(photoSidecarHeader + ` xmp:Rating="4">` + photoSidecarFooter)
				write, writeErr := blobs.WriteDetailedContext(ctx, bytes.NewReader(replacement))
				require.NoError(t, writeErr)
				encoding, encodingErr := write.EncodingName()
				require.NoError(t, encodingErr)
				_, _, replaceErr := catalog.ReplaceContent(ctx, sidecar.ID, sidecar.Revision, write.Hash, write.Size, "application/rdf+xml", store.BlobPhysical{Encoding: encoding, StoredBytes: write.StoredSize, PackEligible: write.PackEligible, Created: write.Created})
				require.NoError(t, replaceErr)
				_, applyErr = catalog.InitializePhotoSidecar(ctx, target)
				require.ErrorIs(t, applyErr, store.ErrStaleRevision)
				_, extractErr := BackfillSourceMetadataTargets(ctx, catalog, blobs, t.TempDir(), []store.SourceMetadataTarget{{SourceSHA256: write.Hash, Size: write.Size}})
				require.NoError(t, extractErr)
				next, listErr := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
				require.NoError(t, listErr)
				require.Len(t, next, 1)
				_, applyErr = catalog.InitializePhotoSidecar(ctx, next[0])
				require.NoError(t, applyErr)
			}
			stream, _, err := blobs.OpenStreamContext(ctx, sidecar.BlobHash)
			require.NoError(t, err)
			original, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			assert.Equal(t, packet, original)
			require.NoError(t, restored.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoSidecarImportInitialization(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	group := store.PhotoImportGroup{DestinationID: catalog.RootID()}
	for _, file := range []struct{ name, role, mime, payload string }{
		{"capture.ARW", store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"},
		{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"},
		{"capture.XMP", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmp:Rating="4" xmp:Label="Red"><dc:description>River</dc:description>` + photoSidecarFooter},
	} {
		receipt, writeErr := blobs.WriteDetailedContext(ctx, strings.NewReader(file.payload))
		require.NoError(t, writeErr)
		encoding, encodingErr := receipt.EncodingName()
		require.NoError(t, encodingErr)
		group.Members = append(group.Members, store.PhotoImportMember{Name: file.name, Role: file.role, BlobHash: receipt.Hash, Size: receipt.Size, MediaType: file.mime, OriginalPath: filepath.Join(root, "camera", file.name), Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created}})
	}
	run, err := catalog.BeginIngest(ctx, "photo-import", filepath.Join(root, "camera"))
	require.NoError(t, err)
	imported, err := catalog.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	require.True(t, imported.Added)
	require.Len(t, imported.Asset.Files, 3)
	pending, err := catalog.MissingSourceMetadataTargetsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
	require.NoError(t, err)
	_, err = BackfillSourceMetadataTargets(ctx, catalog, blobs, t.TempDir(), pending)
	require.NoError(t, err)
	sidecars, err := catalog.MissingPhotoSidecarsAfter(ctx, SourceMetadataExtractorFingerprint, "", 10)
	require.NoError(t, err)
	require.Len(t, sidecars, 1)
	receipt, err := catalog.InitializePhotoSidecar(ctx, sidecars[0])
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ReceiptID)
	asset, err := catalog.PhotoAssetForNode(ctx, imported.Nodes[0].ID)
	require.NoError(t, err)
	for _, file := range asset.Files {
		if file.Role == store.PhotoRoleRAW {
			assert.Equal(t, 4, file.Rating)
			assert.Equal(t, "red", file.Label)
			assert.Equal(t, "River", file.Caption)
			assert.Equal(t, int64(2), file.Revision)
		}
		if file.Role == store.PhotoRoleImage {
			assert.Equal(t, 0, file.Rating)
			assert.Equal(t, int64(1), file.Revision)
		}
	}
}

func TestPhotoSidecarPreservesAuthoredTextThroughExtraction(t *testing.T) {
	t.Parallel()
	packet := []byte(photoSidecarHeader + `><dc:description>` + "\n  River\n" + `</dc:description><dc:creator><rdf:Seq><rdf:li> Creator </rdf:li></rdf:Seq></dc:creator><dc:rights> Copyright </dc:rights>` + photoSidecarFooter)
	catalog, blobs, photo, sidecar, _ := photoSidecarFixture(t, packet)
	_, err := BackfillSourceMetadataTargets(t.Context(), catalog, blobs, t.TempDir(), []store.SourceMetadataTarget{{SourceSHA256: sidecar.BlobHash, Size: sidecar.Size}})
	require.NoError(t, err)
	targets, err := catalog.MissingPhotoSidecarsAfter(t.Context(), SourceMetadataExtractorFingerprint, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	receipt, err := catalog.InitializePhotoSidecar(t.Context(), targets[0])
	require.NoError(t, err)
	require.Len(t, receipt.After, 1)
	assert.Equal(t, "\n  River\n", receipt.After[0].Values.Caption)
	assert.Equal(t, " Creator ", receipt.After[0].Values.Creator)
	assert.Equal(t, " Copyright ", receipt.After[0].Values.Copyright)
	asset, err := catalog.PhotoAssetForNode(t.Context(), photo.ID)
	require.NoError(t, err)
	for _, file := range asset.Files {
		if file.ID == targets[0].FileID {
			assert.Equal(t, "\n  River\n", file.Caption)
			return
		}
	}
	t.Fatal("initialized photo file is missing from the asset")
}
