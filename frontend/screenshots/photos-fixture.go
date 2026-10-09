//go:build ignore

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		panic("usage: photos-fixture <vault>")
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
		width, height := 480, 320
		if index%3 == 0 {
			width, height = 320, 480
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
		metadata, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{Raw: date, Normalized: date, Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneOmitted}}}}})
		check(err)
		if index == 47 {
			metadata, _, err = document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{}})
			check(err)
		}
		samples[index].metadata = metadata
	}
	for index := range 10_000 {
		sampleIndex := index % 10
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
	check(s.Checkpoint(ctx))
	fmt.Println("seeded 10000 synthetic photos")
}
