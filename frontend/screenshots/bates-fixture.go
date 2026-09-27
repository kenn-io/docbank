//go:build ignore

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		panic("usage: bates-fixture <vault>")
	}
	ctx := context.Background()
	layout := home.Layout{Root: os.Args[1]}
	check(layout.Ensure())
	s, err := store.Open(layout.DBPath())
	check(err)
	defer func() { check(s.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(s), layout.BlobsDir())
	check(err)
	defer func() { check(blobs.Close()) }()

	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	for page := 1; page <= 8; page++ {
		pdf.AddPage()
		pdf.SetFont("Helvetica", "", 12)
		pdf.Text(72, 72, fmt.Sprintf("Synthetic source page %d", page))
	}
	var pdfBuffer bytes.Buffer
	check(pdf.Output(&pdfBuffer))
	pdfHash, pdfSize, err := blobs.Write(bytes.NewReader(pdfBuffer.Bytes()))
	check(err)
	node, err := s.CreateFile(ctx, s.RootID(), "Synthetic-review.pdf", pdfHash, pdfSize, "application/pdf")
	check(err)
	source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: pdfSize}
	request := store.PageJobRequest{
		NodeID: node.ID, Revision: node.Revision, Source: source, Pages: []int{3, 6}, DPI: 144,
		RuntimeFingerprint: digest("synthetic renderer"),
	}
	job, err := s.QueuePageJob(ctx, uuid.NewString(), request)
	check(err)
	claim, err := s.ClaimPageJob(ctx)
	check(err)
	frames := make([]document.PageFrameV1, 8)
	for page := range frames {
		frames[page], err = document.NewPDFPageFrame(source, page+1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		check(err)
	}
	check(s.PublishPageFrames(ctx, claim, frames))
	check(s.CancelPageJob(ctx, job.ID, request.Binding()))

	snapshotID := uuid.NewString()
	occurrence := strings.Repeat("a", 32)
	_, err = s.SealCollectionSnapshot(ctx, store.SnapshotSealRequest{SnapshotID: snapshotID, Members: []store.CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
		BlobSHA256: node.BlobHash, Size: pdfSize, FamilyID: occurrence, FamilyOrder: 1,
		DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "pdf",
		SourcePageCount: 8, SelectedSourcePages: []int{3, 6}, SelectedPDFSHA256: node.BlobHash,
	}}})
	check(err)

	manifest := []byte(`{"synthetic":"bates"}`)
	manifestHash, manifestSize, err := blobs.Write(bytes.NewReader(manifest))
	check(err)
	_, err = s.CreateFile(ctx, s.RootID(), "Synthetic-Bates-manifest.json", manifestHash, manifestSize, "application/json")
	check(err)
	emptyJSONHash := digest("{}")
	_, err = s.CreatePackage(ctx, store.PackageRequest{
		PackageID: uuid.NewString(), SnapshotID: snapshotID, Direction: "produced",
		PackageName: "Synthetic-Bates-production", PartyLabel: "Synthetic recipient",
		ProfileJSON: "{}", ProfileSHA256: emptyJSONHash, MappingJSON: "{}", MappingSHA256: emptyJSONHash,
		ManifestSHA256: digest("synthetic Bates logical manifest"), ManifestBlobSHA256: manifestHash,
		State: "sealed", Volumes: []store.PackageVolume{{Ordinal: 1, VolumeName: "VOL001", DeclaredRoot: "VOL001", MappedRoot: "VOL001", ResolvedRootSHA256: digest("synthetic volume")}},
	})
	check(err)
	check(s.Checkpoint(ctx))
	fmt.Println("seeded synthetic Bates package")
}
