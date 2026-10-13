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
	mapGPS := flag.Bool("map", false, "Include synthetic GPS locations")
	unavailable := flag.Bool("unavailable", false, "Include an undecodable photo")
	rejects := flag.Bool("rejects", false, "Include confirmed rejects")
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
	blobs, err := blob.New(store.NewPackCatalog(s), layout.BlobsDir())
	check(err)
	defer func() { check(blobs.Close()) }()
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	check(err)
	type sample struct {
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
		samples[index] = sample{hash: hash, size: size, canonical: canonical, physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize}}
		date := fmt.Sprintf("2022-06-15T%02d:00:00", index%10)
		if index >= 10 {
			date = fmt.Sprintf("%04d-%02d-15T12:00:00", 2026-(index-10)/4, 12-(index-10)%4)
		}
		if index == 46 {
			date = "2024-02-29T23:30:00"
		}
		camera, model, lens := "Canon", "Canon EOS R6", "RF 24-70mm F2.8"
		if index%2 == 1 {
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
	for index := range count {
		if *unavailable && index == count-1 {
			hash, size, err := blobs.Write(bytes.NewReader([]byte("synthetic undecodable JPEG")))
			check(err)
			_, err = s.CreateFile(ctx, s.RootID(), "Synthetic-unavailable.jpg", hash, size, "image/jpeg")
			check(err)
			continue
		}
		sampleIndex := index % 10
		if *mapGPS {
			sampleIndex = index % 15
		}
		if index >= 9000 {
			sampleIndex = 10 + index%38
		}
		item := samples[sampleIndex]
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("Synthetic-photo-%05d.jpg", index+1), item.hash, item.size, "image/jpeg")
		check(err)
		if !samples[sampleIndex].published {
			_, err = processing.BackfillSourceMetadataTargets(ctx, s, blobs, layout.Root, []store.SourceMetadataTarget{{SourceSHA256: item.hash, Size: item.size}})
			check(err)
			fingerprint := sha256.Sum256([]byte("synthetic photo fixture"))
			_, err = s.PublishSourceMetadata(ctx, item.hash, hex.EncodeToString(fingerprint[:]), item.metadata)
			check(err)
			samples[sampleIndex].published = true
		}
		_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, item.canonical, &item.physical)
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
	check(s.Checkpoint(ctx))
	fmt.Printf("seeded %d synthetic photos\n", count)
}

func seedRejects(ctx context.Context, s *store.Store) {
	value, err := query.Parse([]byte(`{"sort":{"field":"name","direction":"asc"}}`))
	check(err)
	page, err := s.ListPhotoAssets(ctx, store.PhotoBrowseRequest{Query: value}, nil)
	check(err)
	var targets []store.PhotoAuthoredTarget
	for _, row := range page.Items[:3] {
		asset, err := s.PhotoAssetByID(ctx, row.AssetID)
		check(err)
		targets = append(targets, store.PhotoAuthoredTarget{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
	}
	_, err = s.EditPhotoAuthored(ctx, targets)
	check(err)
	member, err := s.PhotoAssetByID(ctx, page.Items[3].AssetID)
	check(err)
	_, err = s.DetachPhotoFile(ctx, member.ID, member.Revision, member.Files[0].ID, store.PhotoDetachOptions{})
	check(err)
	asset, err := s.PhotoAssetByID(ctx, page.Items[2].AssetID)
	check(err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, member.Files[0].NodeID, store.PhotoRoleImage, nil)
	check(err)
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &targets[2].FileID)
	check(err)
	node, err := s.NodeByID(ctx, member.Files[0].NodeID)
	check(err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	check(err)
}
