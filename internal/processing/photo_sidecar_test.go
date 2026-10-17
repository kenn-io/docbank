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
		name, packet    string
		want            store.PhotoAuthored
		confirmed       store.PhotoAuthoredFields
		invalid         bool
		ignored         *store.PhotoAuthoredFields
		keywordsIgnored bool
		keywords        []string
	}{
		{name: "rejection overrides RDF pick resource", packet: photoSidecarHeader + `><xmp:Rating>-1</xmp:Rating><ts:Pick rdf:resource="https://example.org/pick"/>` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}, ignored: new(store.PhotoAuthoredFields(0)), confirmed: store.PhotoConfirmedFlag},
		{name: "description value resource ignored", packet: photoSidecarHeader + ` xmp:Rating="2"><dc:description><rdf:value rdf:resource="https://example.com/caption"/></dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 2}, ignored: new(store.PhotoConfirmedCaption), confirmed: store.PhotoConfirmedRating},
		{name: "description alternative resource ignored", packet: photoSidecarHeader + ` xmp:Rating="2"><dc:description><rdf:Alt><rdf:li xml:lang="x-default" rdf:resource="https://example.com/caption"/></rdf:Alt></dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 2}, ignored: new(store.PhotoConfirmedCaption), confirmed: store.PhotoConfirmedRating},
		{name: "description and keyword resources ignored", packet: photoSidecarHeader + ` xmp:Rating="2"><dc:description rdf:resource="https://example.com/caption"/><dc:subject><rdf:Bag><rdf:li rdf:resource="https://example.com/keyword"/></rdf:Bag></dc:subject>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 2}, ignored: new(store.PhotoConfirmedCaption), keywordsIgnored: true, confirmed: store.PhotoConfirmedRating},
		{name: "BOM XML and xpacket prolog", packet: "\xef\xbb\xbf" + `<?xml version="1.0"?><?xpacket begin=""?>` + photoSidecarHeader + ` xmp:Rating="4">` + photoSidecarFooter + `<?xpacket end="w"?>`, want: store.PhotoAuthored{Rating: 4}, confirmed: store.PhotoConfirmedRating},
		{name: "repeated values", packet: photoSidecarHeader + `><xmp:Rating><rdf:value>0</rdf:value><rdf:value>5</rdf:value></xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text before value", packet: photoSidecarHeader + `><xmp:Rating>0<rdf:value>5</rdf:value></xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text after value", packet: photoSidecarHeader + `><xmp:Rating><rdf:value>5</rdf:value>0</xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "text inside Alt", packet: photoSidecarHeader + `><dc:description><rdf:Alt>Caption<rdf:li>Other</rdf:li></rdf:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "padded value", packet: photoSidecarHeader + "><xmp:Rating>\n <rdf:value>5</rdf:value>\n </xmp:Rating>" + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5}, confirmed: store.PhotoConfirmedRating},
		{name: "resource wrapper", packet: photoSidecarHeader + `><dc:creator rdf:parseType="Resource"><rdf:value>Author</rdf:value></dc:creator>` + photoSidecarFooter, want: store.PhotoAuthored{Creator: "Author"}, confirmed: store.PhotoConfirmedCreator},
		{name: "keyword names preserve spaces and skip empty", packet: photoSidecarHeader + `><dc:subject><rdf:Bag><rdf:li> Travel </rdf:li><rdf:li></rdf:li><rdf:li>  </rdf:li><rdf:li>Travel</rdf:li></rdf:Bag></dc:subject>` + photoSidecarFooter, keywords: []string{"  ", " Travel ", "Travel"}},
		{name: "empty", packet: photoSidecarHeader + `>` + photoSidecarFooter, confirmed: 0},
		{name: "numeric zero", packet: photoSidecarHeader + ` ts:Rotation="0" xmp:Rating="0">` + photoSidecarFooter, want: store.PhotoAuthored{}, confirmed: store.PhotoConfirmedRotation},
		{name: "empty containers", packet: photoSidecarHeader + "><dc:description>\n <rdf:Alt>\n </rdf:Alt>\n</dc:description><dc:creator>\n <rdf:Seq>\n </rdf:Seq>\n</dc:creator><dc:rights><rdf:Alt/></dc:rights>" + photoSidecarFooter, confirmed: 0},
		{name: "blank scalar", packet: photoSidecarHeader + "><dc:description> \n\t </dc:description><dc:creator> </dc:creator><dc:rights>\n</dc:rights>" + photoSidecarFooter, confirmed: 0},
		{name: "blank selected items", packet: photoSidecarHeader + `><dc:description><rdf:Alt><rdf:li>Other</rdf:li><rdf:li xml:lang="x-default"> </rdf:li></rdf:Alt></dc:description><dc:creator><rdf:Seq><rdf:li> </rdf:li><rdf:li>Other</rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li> </rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, confirmed: 0},
		{name: "blank attributes", packet: photoSidecarHeader + ` dc:description=" &#10; " dc:creator=" " dc:rights="&#9;">` + photoSidecarFooter, confirmed: 0},
		{name: "padded attributes", packet: photoSidecarHeader + ` dc:description="&#10; River &#10;" dc:creator=" Creator " dc:rights=" Copyright ">` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "\n River \n", Creator: " Creator ", Copyright: " Copyright "}, confirmed: store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedCopyright},
		{name: "duplicate blank property", packet: photoSidecarHeader + ` dc:description=" "><dc:description>Caption</dc:description>` + photoSidecarFooter, invalid: true},
		{name: "attributes", packet: photoSidecarHeader + ` xmp:Rating="5" xmp:Label="Red" ts:Pick="pick" ts:Rotation="90">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 5, Label: "red", Flag: "pick", Rotation: 90}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedLabel | store.PhotoConfirmedFlag | store.PhotoConfirmedRotation},
		{name: "rejection sentinel", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="pick">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}, confirmed: store.PhotoConfirmedFlag},
		{name: "padded rejection sentinel", packet: photoSidecarHeader + ` xmp:Rating=" -01 " ts:Pick=" pick ">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}, confirmed: store.PhotoConfirmedFlag},
		{name: "namespace aliases", packet: `<x:xmpmeta xmlns:x="adobe:ns:meta/"><r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:a="http://ns.adobe.com/xap/1.0/"><r:Description a:Rating="3"/></r:RDF></x:xmpmeta>`, want: store.PhotoAuthored{Rating: 3}, confirmed: store.PhotoConfirmedRating},
		{name: "wrong namespace", packet: photoSidecarHeader + ` xmlns:bad="https://example.org/xmp" bad:Rating="5" bad:Pick="pick">` + photoSidecarFooter, confirmed: 0},
		{name: "rdf text", packet: photoSidecarHeader + `><xmp:Rating>4</xmp:Rating><dc:description><rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="x-default">River &amp; sky</rdf:li></rdf:Alt></dc:description><dc:creator><rdf:Seq><rdf:li>Creator A</rdf:li><rdf:li>Creator B</rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li>Copyright example</rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River & sky", Creator: "Creator A", Copyright: "Copyright example"}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedCopyright},
		{name: "custom label", packet: photoSidecarHeader + ` xmp:Rating="4" xmp:Label="Approved"><dc:description>Caption</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "Caption"}, ignored: new(store.PhotoConfirmedLabel), confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption},
		{name: "oversized caption", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>` + oversized + `</dc:description><dc:creator>Creator</dc:creator><dc:rights>Copyright</dc:rights>` + photoSidecarFooter, invalid: true},
		{name: "maximum text bytes", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Pick="pick"><dc:description>` + bounded + `</dc:description><dc:creator>` + bounded + `</dc:creator><dc:rights>` + bounded + `</dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Flag: "pick", Caption: bounded, Creator: bounded, Copyright: bounded}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedFlag | store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedCopyright},
		{name: "out-of-range rating", packet: photoSidecarHeader + ` xmp:Rating="6" xmp:Label="Red">` + photoSidecarFooter, want: store.PhotoAuthored{Label: "red"}, confirmed: store.PhotoConfirmedLabel},
		{name: "real rating", packet: photoSidecarHeader + ` xmp:Rating="3.0">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 3}, confirmed: store.PhotoConfirmedRating},
		{name: "fractional rating", packet: photoSidecarHeader + ` xmp:Rating="2.5" xmp:Label="Red">` + photoSidecarFooter, want: store.PhotoAuthored{Label: "red"}, confirmed: store.PhotoConfirmedLabel},
		{name: "bad rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="45"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption},
		{name: "malformed rotation", packet: photoSidecarHeader + ` xmp:Rating="4" ts:Rotation="bad">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}, confirmed: store.PhotoConfirmedRating},
		{name: "authored whitespace", packet: photoSidecarHeader + `><dc:description>` + "\n  River\n" + `</dc:description><dc:creator><rdf:Seq><rdf:li>  Creator  </rdf:li></rdf:Seq></dc:creator><dc:rights><rdf:Alt><rdf:li xml:lang="x-default"> Copyright </rdf:li></rdf:Alt></dc:rights>` + photoSidecarFooter, want: store.PhotoAuthored{Caption: "\n  River\n", Creator: "  Creator  ", Copyright: " Copyright "}, confirmed: store.PhotoConfirmedCaption | store.PhotoConfirmedCreator | store.PhotoConfirmedCopyright},
		{name: "unsupported pick", packet: photoSidecarHeader + ` xmp:Rating="2" ts:Pick="yes">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 2}, confirmed: store.PhotoConfirmedRating},
		{name: "namespace scopes", packet: photoSidecarHeader + ` xmlns:p="p"><p:History xmlns:p="https://example.org/inner" xmlns:xmp="https://example.org/custom"><xmp:Rating>Other</xmp:Rating><Plain xmlns="https://example.org/default"><Child xmlns=""/></Plain></p:History><p:History/><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 3}, confirmed: store.PhotoConfirmedRating},
		{name: "undeclared attribute prefix", packet: photoSidecarHeader + ` bad:Credit="Credit">` + photoSidecarFooter, invalid: true},
		{name: "expired prefix scope", packet: photoSidecarHeader + `><a xmlns:bad="https://example.org/custom"/><bad:Credit/>` + photoSidecarFooter, invalid: true},
		{name: "duplicate namespace", packet: photoSidecarHeader + ` xmlns:p="https://example.org/one" xmlns:p="https://example.org/two">` + photoSidecarFooter, invalid: true},
		{name: "attribute namespace collision", packet: photoSidecarHeader + ` xmlns:a="https://example.org/custom" xmlns:b="https://example.org/custom" a:Credit="One" b:Credit="Two">` + photoSidecarFooter, invalid: true},
		{name: "reserved xml binding", packet: photoSidecarHeader + ` xmlns:xml="https://example.org/custom">` + photoSidecarFooter, invalid: true},
		{name: "reserved xmlns prefix", packet: photoSidecarHeader + ` xmlns:xmlns="https://example.org/custom">` + photoSidecarFooter, invalid: true},
		{name: "reserved xmlns URI", packet: photoSidecarHeader + ` xmlns:p="http://www.w3.org/2000/xmlns/">` + photoSidecarFooter, invalid: true},
		{name: "reserved xml URI", packet: photoSidecarHeader + ` xmlns:p="http://www.w3.org/XML/1998/namespace">` + photoSidecarFooter, invalid: true},
		{name: "empty prefixed binding", packet: photoSidecarHeader + ` xmlns:p="">` + photoSidecarFooter, invalid: true},
		{name: "undeclared prefix", packet: photoSidecarHeader + `><bad:Credit>Credit</bad:Credit>` + photoSidecarFooter, invalid: true},
		{name: "malformed suffix", packet: photoSidecarHeader + ` xmp:Rating="5">` + photoSidecarFooter + `<`, invalid: true},
		{name: "two roots", packet: strings.Repeat(photoSidecarHeader+`>`+photoSidecarFooter, 2), invalid: true},
		{name: "no RDF", packet: `<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`, invalid: true},
		{name: "wrong RDF value namespace", packet: photoSidecarHeader + `><dc:description><bad:Alt xmlns:bad="https://example.org/rdf"><bad:li>Hidden</bad:li></bad:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "rejection with unsupported pick", packet: photoSidecarHeader + ` xmp:Rating="-1" ts:Pick="unknown">` + photoSidecarFooter, want: store.PhotoAuthored{Flag: "reject"}, confirmed: store.PhotoConfirmedFlag},
		{name: "duplicate", packet: photoSidecarHeader + ` xmp:Rating="5"><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter, invalid: true},
		{name: "other subject", packet: photoSidecarHeader + ` rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="thumbnail" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}, confirmed: store.PhotoConfirmedRating},
		{name: "same default subject", packet: photoSidecarHeader + ` rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption},
		{name: "nested subject", packet: photoSidecarHeader + ` xmp:Rating="4"><metadata><rdf:RDF><rdf:Description xmp:Rating="1"/></rdf:RDF></metadata>` + photoSidecarFooter, invalid: true},
		{name: "identifier kinds differ", packet: photoSidecarHeader + ` rdf:nodeID="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="photo" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4}, confirmed: store.PhotoConfirmedRating},
		{name: "matching node ID", packet: photoSidecarHeader + ` rdf:nodeID="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:nodeID="photo" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, want: store.PhotoAuthored{Rating: 4, Caption: "River"}, confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption},
		{name: "ID with empty about", packet: photoSidecarHeader + ` rdf:ID="photo" rdf:about="" xmp:Rating="4"></rdf:Description><rdf:Description rdf:ID="photo" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:description>River</dc:description>` + photoSidecarFooter, invalid: true},
		{name: "same subject duplicate", packet: photoSidecarHeader + ` rdf:about="photo" xmp:Rating="4"></rdf:Description><rdf:Description rdf:about="photo" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1">` + photoSidecarFooter, invalid: true},
		{name: "deep", packet: photoSidecarHeader + `>` + strings.Repeat(`<a>`, 65) + strings.Repeat(`</a>`, 65) + photoSidecarFooter, invalid: true},
		{name: "oversized alternative beside default", packet: photoSidecarHeader + `><dc:description><rdf:Alt><rdf:li xml:lang="fr">` + oversized + `</rdf:li><rdf:li xml:lang="x-default">River</rdf:li></rdf:Alt></dc:description>` + photoSidecarFooter, invalid: true},
		{name: "large editing data", packet: photoSidecarHeader + ` xmp:Rating="5"><edit:Brush xmlns:edit="https://example.org/edit">` + strings.Repeat("x", 2<<20) + `</edit:Brush>` + photoSidecarFooter, invalid: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			packet, err := readPhotoSidecar(t.Context(), []byte(c.packet))
			if err == nil {
				err = packet.Refusal
			}
			if c.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, c.confirmed, packet.Confirmed)
				packet.Confirmed = 0
				assert.Equal(t, c.want, packet.PhotoAuthored)
				if c.keywords != nil {
					assert.Equal(t, c.keywords, packet.Keywords)
				}
				if c.ignored != nil {
					assert.Equal(t, *c.ignored, packet.Ignored)
					assert.Equal(t, c.keywordsIgnored, packet.KeywordsIgnored)
				}
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

type photoGroupFile struct{ name, role, mime, payload string }

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

// ReadPhotoSidecar returns a packet's supported values, refusing one the sync would refuse.
func ReadPhotoSidecar(ctx context.Context, data []byte) (store.PhotoAuthored, error) {
	packet, err := readPhotoSidecar(ctx, data)
	if err == nil {
		err = packet.Refusal
	}
	packet.Confirmed = 0
	return packet.PhotoAuthored, err
}
