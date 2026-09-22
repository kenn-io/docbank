package loadfile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxRetainedManifestBytes = 512 << 20
	maxRetainedManifestLine  = 8 << 20
	maxRetainedManifestFiles = 2_000_000
)

// ReadManifestJSONL accepts only the exact canonical stream retained by
// Manifest.WriteJSONL, under fixed byte and item bounds. The expected digest
// binds the rows and their order to the admitted package.
func ReadManifestJSONL(source io.Reader, expectedSHA256 string) (Manifest, error) {
	if source == nil || !canonical.IsSHA256Hex(expectedSHA256) {
		return Manifest{}, ErrMalformedInput
	}
	limited := &io.LimitedReader{R: source, N: maxRetainedManifestBytes + 1}
	digest := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(limited, digest), maxRetainedManifestLine+1)
	var manifest Manifest
	stage := 0
	lines := 0
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxRetainedManifestLine+1 {
			return Manifest{}, ErrLoadfileLimit
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Manifest{}, fmt.Errorf("%w: manifest line lacks newline", ErrMalformedInput)
			}
			return Manifest{}, fmt.Errorf("read retained manifest: %w", err)
		}
		if len(line) < 3 || len(line) > maxRetainedManifestLine+1 {
			return Manifest{}, ErrMalformedInput
		}
		line = line[:len(line)-1]
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			return Manifest{}, fmt.Errorf("%w: manifest line %d: %w", ErrMalformedInput, lines+1, err)
		}
		if lines == 0 {
			if kind.Kind != "manifest" {
				return Manifest{}, ErrMalformedInput
			}
			header, err := canonical.Decode[manifestHeaderIdentity](line)
			if err != nil || !canonical.IsSHA256Hex(header.ProfileSHA256) || !canonical.IsSHA256Hex(header.MappingSHA256) {
				return Manifest{}, ErrMalformedInput
			}
			manifest.ProfileSHA256, manifest.MappingSHA256, manifest.Mapping =
				header.ProfileSHA256, header.MappingSHA256, header.Mapping
			lines++
			continue
		}
		var rank int
		switch kind.Kind {
		case "volume":
			rank = 1
			item, err := canonical.Decode[manifestItemIdentity[Volume]](line)
			if err != nil || len(manifest.Volumes) == 64 {
				return Manifest{}, ErrMalformedInput
			}
			manifest.Volumes = append(manifest.Volumes, item.Value)
		case "record":
			rank = 2
			item, err := canonical.Decode[manifestItemIdentity[Record]](line)
			if err != nil || len(manifest.Records) == 100_000 {
				return Manifest{}, ErrMalformedInput
			}
			manifest.Records = append(manifest.Records, item.Value)
		case "image":
			rank = 3
			item, err := canonical.Decode[manifestItemIdentity[ImageRef]](line)
			if err != nil || len(manifest.Images) == 1_000_000 {
				return Manifest{}, ErrMalformedInput
			}
			manifest.Images = append(manifest.Images, item.Value)
		case "file":
			rank = 4
			item, err := canonical.Decode[manifestItemIdentity[FileRef]](line)
			if err != nil || len(manifest.Files) == maxRetainedManifestFiles {
				return Manifest{}, ErrMalformedInput
			}
			manifest.Files = append(manifest.Files, item.Value)
		default:
			return Manifest{}, ErrMalformedInput
		}
		if rank < stage {
			return Manifest{}, ErrMalformedInput
		}
		stage = rank
		lines++
	}
	if limited.N == 0 {
		return Manifest{}, ErrLoadfileLimit
	}
	if lines == 0 || hex.EncodeToString(digest.Sum(nil)) != expectedSHA256 {
		return Manifest{}, ErrMalformedInput
	}
	return manifest, nil
}
