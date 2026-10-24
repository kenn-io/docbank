//go:build ignore

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strconv"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	namesakes := flag.Bool("namesakes", false, "Include people with the same name and distinct identities")
	manyNamesakes := flag.Bool("many-namesakes", false, "Include 300 people named Alex")
	faceSearch := flag.Bool("face-search", false, "Include later people-search matches and a rotated namesake preview")
	bursts := flag.Bool("bursts", false, "Include synthetic burst stacks")
	mapGPS := flag.Bool("map", false, "Include synthetic GPS locations")
	unavailable := flag.Bool("unavailable", false, "Include an undecodable photo")
	rejects := flag.Bool("rejects", false, "Include confirmed rejects")
	rotateCover := flag.Bool("rotate-cover", false, "Rotate the album cover for comparison")
	manyKeywords := flag.Bool("many-keywords", false, "Include an assigned keyword beyond the first 1,000")
	pair := flag.Bool("pair", false, "Include a synthetic RAW/JPEG pair")
	currentMetadata := flag.Bool("current-metadata", false, "Mark synthetic source metadata as produced by the current extractor")
	staleBurstMember := flag.Bool("stale-burst-member", false, "Leave one synthetic burst member with stale source metadata")
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 || len(args) > 2 {
		panic("usage: photos-fixture [-unavailable] [--rejects] <vault> [count]")
	}
	count := 10_000
	if len(args) == 2 {
		var err error
		count, err = strconv.Atoi(args[1])
		check(err)
		if count < 1 {
			panic("count must be positive")
		}
	}
	ctx := context.Background()
	layout := home.Layout{Root: args[0]}
	check(layout.Ensure())
	s, err := store.Open(layout.DBPath())
	check(err)
	defer func() { check(s.Close()) }()
	var pairedPerson string
	if *namesakes {
		for _, email := range []string{"alex-a@example.test", "alex-b@example.test"} {
			person, err := s.CreatePerson(ctx, "Alex Smith", "operator")
			check(err)
			if pairedPerson == "" {
				pairedPerson = person.PersonID
			}
			_, err = s.AddPersonIdentity(ctx, person.PersonID, person.Revision, store.PersonIdentity{
				Kind: "email", ValueDisplay: email, Origin: "operator", EvidenceKind: "operator_assertion",
				EvidenceID: "synthetic-chooser", Confidence: "operator_asserted",
			})
			check(err)
		}
	}
	if *manyNamesakes {
		for range 300 {
			_, err := s.CreatePerson(ctx, "Alex", "operator")
			check(err)
		}
	}
	if *faceSearch {
		for index := range 100 {
			_, err := s.CreatePerson(ctx, fmt.Sprintf("Aaron %03d Smith", index), "operator")
			check(err)
		}
		for _, name := range []string{"Smith", "Robin", "Robin"} {
			_, err := s.CreatePerson(ctx, name, "operator")
			check(err)
		}
	}
	blobs, err := blob.New(store.NewPackCatalog(s), layout.BlobsDir())
	check(err)
	defer func() { check(blobs.Close()) }()
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	check(err)
	type sample struct {
		source    []byte
		hash      string
		size      int64
		canonical []byte
		physical  store.BlobPhysical
		metadata  []byte
		published bool
	}
	samples := make([]sample, 48)
	for index := range samples {
		width, height := 1440, 960
		if index%3 == 0 {
			width, height = 960, 1440
		}
		frame := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				shade := color.RGBA{uint8(80 + index*3), uint8(150 + y*60/height), uint8(180 + index), 255}
				if float64(y) > float64(height)*0.42+math.Sin(float64(x)/65+float64(index))*55 {
					shade = color.RGBA{uint8(35 + index*2), uint8(80 + index), uint8(100 + index), 255}
				}
				if float64(y) > float64(height)*0.72+math.Cos(float64(x)/90+float64(index))*30 {
					shade = color.RGBA{uint8(40 + index), uint8(120 + index), uint8(130 + index), 255}
				}
				frame.SetRGBA(x, y, shade)
			}
		}
		if *faceSearch || *namesakes {
			centers := [][2]float64{{0.25, 0.4}, {0.75, 0.4}}
			if *namesakes {
				centers = append(centers, [2]float64{0.5, 0.5})
			}
			for y := 0; y < height; y++ {
				for x := 0; x < width; x++ {
					for faceIndex, center := range centers {
						dx, dy := float64(x)/float64(width)-center[0], float64(y)/float64(height)-center[1]
						if dx*dx/0.01+dy*dy/0.01 < 1 {
							shade := color.RGBA{240, 180, 100, 255}
							if faceIndex == 1 {
								shade = color.RGBA{120, 190, 240, 255}
							}
							if (math.Abs(dx-0.035) < 0.012 || math.Abs(dx+0.035) < 0.012) && math.Abs(dy+0.03) < 0.015 || math.Abs(dx) < 0.045 && math.Abs(dy-0.06) < 0.01 {
								shade = color.RGBA{25, 30, 40, 255}
							}
							frame.SetRGBA(x, y, shade)
						}
					}
				}
			}
		}
		var encoded bytes.Buffer
		check(jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 85}))
		hash, size, err := blobs.Write(bytes.NewReader(encoded.Bytes()))
		check(err)
		product, err := processing.ProduceVisualPreviewForRecipe(ctx, bytes.NewReader(encoded.Bytes()), processing.VisualPreviewTarget{SourceSHA256: hash, Size: size, MediaType: "image/jpeg"}, recipe)
		check(err)
		receipt, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(product.Output))
		check(err)
		encoding, err := receipt.EncodingName()
		check(err)
		canonical, _, err := document.MarshalVisualPreviewV1(product.Preview)
		check(err)
		samples[index] = sample{source: encoded.Bytes(), hash: hash, size: size, canonical: canonical, physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize}}
		date := fmt.Sprintf("2022-06-15T%02d:00:00", index%10)
		if *bursts {
			date = fmt.Sprintf("2022-06-15T12:%02d:%02d", (index/3*10)/60, (index/3*10)%60+index%3*2)
		}
		if index >= 10 && !*bursts {
			date = fmt.Sprintf("%04d-%02d-15T12:00:00", 2026-(index-10)/4, 12-(index-10)%4)
		}
		if index == 46 {
			date = "2024-02-29T23:30:00"
		}
		camera, model, lens := "Canon", "Canon EOS R6", "RF 24-70mm F2.8"
		cameraIndex := index
		if *bursts {
			cameraIndex = index / 3
		}
		if cameraIndex%2 == 1 {
			camera, model, lens = "Nikon", "Nikon Z6", "NIKKOR Z 35mm F1.8"
		}
		fields := []document.SourceMetadataFieldV1{
			{Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{Raw: date, Normalized: date, Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneOmitted}}},
			{Key: "image.exif.camera_make", Namespace: "image.exif", SourceField: "Make", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &camera}},
			{Key: "image.exif.camera_model", Namespace: "image.exif", SourceField: "Model", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &model}},
			{Key: "image.exif.lens_model", Namespace: "image.exif", SourceField: "LensModel", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &lens}},
			{Key: "image.exif.iso", Namespace: "image.exif", SourceField: "PhotographicSensitivity", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(400))}},
			{Key: "image.exif.f_number", Namespace: "image.exif", SourceField: "FNumber", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: new(2.8)}},
			{Key: "image.exif.exposure_time_seconds", Namespace: "image.exif", SourceField: "ExposureTime", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: new(0.004)}},
			{Key: "image.exif.focal_length_mm", Namespace: "image.exif", SourceField: "FocalLength", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: new(50.0)}},
		}
		location, located := [2]string{"48.8566", "2.3522"}, true
		if *mapGPS {
			locations := [][2]string{{"48.8566", "2.3522"}, {"48.8666", "2.3622"}, {"48.8766", "2.3722"}, {"48.8866", "2.3822"}, {"48.8966", "2.3922"}, {"48.9066", "2.4022"}, {"0", "12"}, {"35.67", "139.65"}, {"40.748816666666667", "-73.98565555555555"}, {"40.748816666666667", "-73.98565555555555"}, {"90", "0"}, {"-90", "0"}, {"0", "-180"}, {"0", "180"}}
			located = index < len(locations)
			if located {
				location = locations[index]
			}
		}
		if located {
			fields = append(fields,
				document.SourceMetadataFieldV1{Key: "image.exif.gps_latitude", Namespace: "image.exif", SourceField: "GPSLatitude", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(location[0])}},
				document.SourceMetadataFieldV1{Key: "image.exif.gps_longitude", Namespace: "image.exif", SourceField: "GPSLongitude", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(location[1])}},
			)
		}
		metadata, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: fields})
		check(err)
		if index == 47 {
			metadata, _, err = document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{}})
			check(err)
		}
		samples[index].metadata = metadata
	}
	var run store.IngestRun
	if *bursts {
		run, err = s.BeginIngest(ctx, "cli", "synthetic photos")
		check(err)
	}
	var firstAssetID string
	for index := range count {
		if *unavailable && index == count-1 {
			hash, size, err := blobs.Write(bytes.NewReader([]byte("synthetic undecodable JPEG")))
			check(err)
			_, err = s.CreateFile(ctx, s.RootID(), "Synthetic-unavailable.jpg", hash, size, "image/jpeg")
			check(err)
			continue
		}
		sampleIndex := index % 10
		if *bursts {
			sampleIndex = index % 48
		}
		if *mapGPS {
			sampleIndex = index % 15
		}
		if index >= 9000 {
			sampleIndex = 10 + index%38
		}
		item := samples[sampleIndex]
		name := fmt.Sprintf("Synthetic-photo-%05d.jpg", index+1)
		var node store.Node
		if *bursts {
			node, err = s.IngestFileExact(ctx, run, s.RootID(), name, item.hash, item.size, "image/jpeg", "/synthetic/shots/"+name, "")
		} else {
			node, err = s.CreateFile(ctx, s.RootID(), name, item.hash, item.size, "image/jpeg")
		}
		check(err)
		if (*rotateCover || *manyKeywords || *faceSearch) && index == 0 {
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			check(err)
			firstAssetID = asset.ID
		}
		if !samples[sampleIndex].published {
			if *currentMetadata && (!*staleBurstMember || sampleIndex != 1) {
				_, err = s.PublishSourceMetadata(ctx, item.hash, processing.SourceMetadataExtractorFingerprint, item.metadata)
			} else {
				_, err = processing.BackfillSourceMetadataTargets(ctx, s, blobs, layout.Root, []store.SourceMetadataTarget{{SourceSHA256: item.hash, Size: item.size}})
				check(err)
				stale := sha256.Sum256([]byte("synthetic photo fixture"))
				_, err = s.PublishSourceMetadata(ctx, item.hash, hex.EncodeToString(stale[:]), item.metadata)
			}
			check(err)
			samples[sampleIndex].published = true
		}
		_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, item.canonical, &item.physical)
		check(err)
		if (*pair && index == 0) || (*namesakes && index == 1) {
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			check(err)
			rawHash, rawSize, rawMIME := item.hash, item.size, "application/octet-stream"
			if *namesakes && index == 1 {
				// This portrait RAW preview is framed differently from its landscape JPEG.
				rawBytes := mediatest.TIFFWithEmbeddedJPEG(1, mediatest.TIFFJPEGPreview{Data: samples[0].source})
				rawHash, rawSize, err = blobs.Write(bytes.NewReader(rawBytes))
				check(err)
				rawMIME = "image/x-canon-cr2"
			}
			raw, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("Synthetic-photo-%05d.cr2", index+1), rawHash, rawSize, rawMIME)
			check(err)
			if *namesakes && index == 1 {
				singleton, err := s.PhotoAssetForNode(ctx, raw.ID)
				check(err)
				_, err = s.DetachPhotoFile(ctx, singleton.ID, singleton.Revision, singleton.Files[0].ID, store.PhotoDetachOptions{})
				check(err)
			}
			asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, store.PhotoRoleRAW, nil)
			check(err)
			if *namesakes && index == 1 {
				_, _, err = s.CreatePhotoFace(ctx, raw.CurrentVersionID, "00000000-0000-4000-8000-000000000001", document.FaceBox{X: .15, Y: .3, Width: .2, Height: .2}, pairedPerson)
				check(err)
			}

			var display string
			for _, file := range asset.Files {
				if file.NodeID == node.ID {
					display = file.ID
				}
			}
			_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &display)
			check(err)
			_, err = s.CreateTag(ctx, "Portfolio")
			check(err)
		}
	}
	if *namesakes {
		// Two newer unavailable photos keep the paired RAW inside the three-preview bound.
		unavailable, err := s.BeginIngest(ctx, "cli", "unavailable namesake photos")
		check(err)
		for index := range 2 {
			hash, size, err := blobs.Write(bytes.NewReader(fmt.Appendf(nil, "synthetic undecodable JPEG %d", index)))
			check(err)
			name := fmt.Sprintf("Unavailable-namesake-%d.jpg", index)
			node, err := s.IngestFileExact(ctx, unavailable, s.RootID(), name, hash, size, "image/jpeg", "/synthetic/unavailable/"+name, fmt.Sprintf("2026-10-0%dT12:00:00Z", index+1))
			check(err)
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, fmt.Sprintf("00000000-0000-4000-8000-%012d", index+2), document.FaceBox{Width: .2, Height: .2}, pairedPerson)
			check(err)
		}
	}
	if *manyKeywords {
		for index := range 1000 {
			_, err := s.CreateTag(ctx, fmt.Sprintf("Catalog keyword %04d", index))
			check(err)
		}
		tag, err := s.CreateTag(ctx, "ZZ assigned keyword")
		check(err)
		asset, err := s.PhotoAssetByID(ctx, firstAssetID)
		check(err)
		file := asset.Files[0]
		node, err := s.NodeByID(ctx, file.NodeID)
		check(err)
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		check(err)
	}
	album, err := s.CreatePhotoSet(ctx, "Paris walks")
	check(err)
	scope, err := query.Parse([]byte(`{"filters":{"cameras":["Canon EOS R6"]}}`))
	check(err)
	_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, store.PhotoSetSelection{Query: &scope})
	check(err)
	if *rejects {
		if count < 4 {
			panic("--rejects requires at least four photos")
		}
		seedRejects(ctx, s)
	}
	if *rotateCover || *faceSearch {
		first, err := s.PhotoAssetByID(ctx, firstAssetID)
		check(err)
		prepared, err := s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{first.ID}})
		check(err)
		_, err = s.EditPhotoAuthoredBatch(ctx, store.PhotoAuthoredBatch{Targets: editTargets(prepared.Targets), Patch: store.PhotoAuthoredPatch{Rotation: new(90)}})
		check(err)
		if *rotateCover {
			cover := &first.ID
			current, err := s.PhotoSet(ctx, album.ID, "")
			check(err)
			_, err = s.UpdatePhotoSet(ctx, album.ID, current.Revision, nil, nil, &cover)
			check(err)
		}
	}
	check(s.Checkpoint(ctx))
	fmt.Printf("seeded %d synthetic photos\n", count)
}

func seedRejects(ctx context.Context, s *store.Store) {
	value, err := query.Parse([]byte(`{"sort":{"field":"name","direction":"asc"}}`))
	check(err)
	page, err := s.ListPhotoAssets(ctx, store.PhotoBrowseRequest{Query: value}, nil)
	check(err)
	var ids []string
	for _, row := range page.Items[:3] {
		ids = append(ids, row.AssetID)
	}
	prepared, err := s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: ids})
	check(err)
	_, err = s.EditPhotoAuthoredBatch(ctx, store.PhotoAuthoredBatch{Targets: editTargets(prepared.Targets), Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
	check(err)
	member, err := s.PhotoAssetByID(ctx, page.Items[3].AssetID)
	check(err)
	_, err = s.DetachPhotoFile(ctx, member.ID, member.Revision, member.Files[0].ID, store.PhotoDetachOptions{})
	check(err)
	asset, err := s.PhotoAssetByID(ctx, page.Items[2].AssetID)
	check(err)
	displayFileID := asset.DisplayFileID
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, member.Files[0].NodeID, store.PhotoRoleImage, nil)
	check(err)
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, displayFileID)
	check(err)
	node, err := s.NodeByID(ctx, member.Files[0].NodeID)
	check(err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	check(err)
}

func editTargets(prepared []store.PhotoPreparedTarget) []store.PhotoAuthoredEditTarget {
	targets := make([]store.PhotoAuthoredEditTarget, len(prepared))
	for i, target := range prepared {
		targets[i] = store.PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}
	}
	return targets
}
