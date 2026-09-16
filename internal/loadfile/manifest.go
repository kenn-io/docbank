package loadfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"go.kenn.io/docbank/internal/canonical"
)

type Manifest struct {
	ProfileSHA256 string
	MappingSHA256 string
	Mapping       Mapping
	Volumes       []Volume
	Records       []Record
	Images        []ImageRef
	Files         []FileRef
}

type manifestHeaderIdentity struct {
	Kind          string  `json:"kind"`
	ProfileSHA256 string  `json:"profile_sha256"`
	MappingSHA256 string  `json:"mapping_sha256"`
	Mapping       Mapping `json:"mapping"`
}

type manifestItemIdentity[T any] struct {
	Kind  string `json:"kind"`
	Value T      `json:"value"`
}

func (m Manifest) SHA256() (string, error) {
	digest := sha256.New()
	if err := writeManifestJSONL(digest, m); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// WriteJSONL writes the exact normalized stream whose bytes SHA256 binds.
func (m Manifest) WriteJSONL(writer io.Writer) error {
	return writeManifestJSONL(writer, m)
}

func writeManifestJSONL(writer io.Writer, manifest Manifest) error {
	if err := writeManifestLine(writer, manifestHeaderIdentity{
		Kind: "manifest", ProfileSHA256: manifest.ProfileSHA256, MappingSHA256: manifest.MappingSHA256, Mapping: manifest.Mapping,
	}); err != nil {
		return err
	}
	for _, volume := range manifest.Volumes {
		if err := writeManifestLine(writer, manifestItemIdentity[Volume]{Kind: "volume", Value: volume}); err != nil {
			return err
		}
	}
	for _, record := range manifest.Records {
		if err := writeManifestLine(writer, manifestItemIdentity[Record]{Kind: "record", Value: record}); err != nil {
			return err
		}
	}
	for _, image := range manifest.Images {
		if err := writeManifestLine(writer, manifestItemIdentity[ImageRef]{Kind: "image", Value: image}); err != nil {
			return err
		}
	}
	for _, file := range manifest.Files {
		if err := writeManifestLine(writer, manifestItemIdentity[FileRef]{Kind: "file", Value: file}); err != nil {
			return err
		}
	}
	return nil
}

func writeManifestLine(writer io.Writer, value any) error {
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode manifest identity record: %w", err)
	}
	written, err := writer.Write(encoded)
	if err != nil {
		return fmt.Errorf("write manifest identity record: %w", err)
	}
	if written != len(encoded) {
		return fmt.Errorf("write manifest identity record: %w", io.ErrShortWrite)
	}
	written, err = writer.Write([]byte{'\n'})
	if err != nil {
		return fmt.Errorf("terminate manifest identity record: %w", err)
	}
	if written != 1 {
		return fmt.Errorf("terminate manifest identity record: %w", io.ErrShortWrite)
	}
	return nil
}
