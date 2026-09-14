package loadfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

var ErrUnrepresentable = errors.New("invalid_package_profile: value cannot be represented in the declared profile")

const maxLFPLineBytes = 4*MaxFieldValueBytes + 128

func lfpCommandSupported(command string) bool {
	return command == "IM" || command == "##"
}

func ParseLFP(source io.Reader, profile Profile) ([]ImageRef, []Diagnostic, error) {
	if profile.ID != "lfp-ipro-v1" || profile.Field != ',' {
		return nil, nil, ErrInvalidProfile
	}
	decoder, err := Decoder(profile.Encoding)
	if err != nil {
		return nil, nil, err
	}
	images := make([]ImageRef, 0, 100)
	diagnostics := make([]Diagnostic, 0)
	pageOrdinal := 0
	rowOrdinal := 0
	scanner := bufio.NewScanner(decoder(source))
	scanner.Buffer(make([]byte, validationReadSize), maxLFPLineBytes)
	for scanner.Scan() {
		rowOrdinal++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, "##") {
			if !lfpCommandSupported("##") {
				return images, diagnostics, ErrUnrepresentable
			}
			continue
		}
		values := strings.Split(line, ",")
		command := ""
		if len(values) > 0 {
			command = values[0]
		}
		if !lfpCommandSupported(command) {
			return images, diagnostics, fmt.Errorf("%w: unsupported LFP command %q on row %d", ErrUnrepresentable, command, rowOrdinal)
		}
		if len(images) == 1_000_000 {
			return images, diagnostics, ErrLoadfileLimit
		}
		if len(values) != 6 {
			return images, diagnostics, fmt.Errorf("%w: LFP IM row %d has %d fields", ErrMalformedInput, rowOrdinal, len(values))
		}
		for _, value := range values {
			if len(value) > MaxFieldValueBytes {
				return images, diagnostics, fmt.Errorf("%w: LFP row %d field exceeds %d bytes", ErrMalformedInput, rowOrdinal, MaxFieldValueBytes)
			}
		}
		if values[3] != "0" {
			return images, diagnostics, fmt.Errorf("%w: nonzero LFP offset on row %d", ErrUnrepresentable, rowOrdinal)
		}
		group := strings.Split(values[4], ";")
		if len(group) != 4 || !strings.HasPrefix(group[0], "@") || len(group[0]) == 1 || group[2] == "" {
			return images, diagnostics, fmt.Errorf("%w: malformed LFP image group on row %d", ErrMalformedInput, rowOrdinal)
		}
		if group[3] != "2" {
			return images, diagnostics, fmt.Errorf("%w: unsupported LFP image type %q", ErrUnrepresentable, group[3])
		}
		boundary := ""
		documentBreak := false
		switch values[2] {
		case "D":
			boundary, documentBreak = "document", true
		case "C":
			boundary, documentBreak = "child", true
		case "":
		default:
			return images, diagnostics, fmt.Errorf("%w: unsupported LFP boundary %q", ErrUnrepresentable, values[2])
		}
		rotation, parseErr := strconv.Atoi(values[5])
		if parseErr != nil || !validLFPRotation(rotation) {
			return images, diagnostics, fmt.Errorf("%w: invalid LFP rotation on row %d", ErrMalformedInput, rowOrdinal)
		}
		if documentBreak || pageOrdinal == 0 {
			pageOrdinal = 1
		} else {
			pageOrdinal++
		}
		directory := strings.ReplaceAll(group[1], `\`, "/")
		relPath := group[2]
		if directory != "" {
			relPath = strings.TrimSuffix(directory, "/") + "/" + group[2]
		}
		images = append(images, ImageRef{
			ImageKey: values[1], Volume: strings.TrimPrefix(group[0], "@"), RelPath: relPath,
			DocumentBreak: documentBreak, PageOrdinal: pageOrdinal, Boundary: boundary, Rotation: rotation,
		})
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return images, diagnostics, fmt.Errorf("%w: LFP row exceeds %d bytes", ErrMalformedInput, maxLFPLineBytes)
		}
		return images, diagnostics, fmt.Errorf("read LFP input: %w", err)
	}
	return images, diagnostics, nil
}

func WriteLFP(writer io.Writer, images []ImageRef, profile Profile) error {
	if profile.ID != "lfp-ipro-v1" || profile.Field != ',' || profile.Encoding != encodingNameUTF8 {
		return ErrInvalidProfile
	}
	buffered := bufio.NewWriter(writer)
	for index, image := range images {
		if !lfpModelRepresentable(images, index) {
			return ErrUnrepresentable
		}
		boundary, err := lfpBoundary(image)
		if err != nil {
			return err
		}
		directory, filename := path.Split(image.RelPath)
		directory = strings.TrimSuffix(directory, "/")
		for _, value := range []string{image.ImageKey, image.Volume, directory, filename} {
			if !lfpValueRepresentable(value) {
				return ErrUnrepresentable
			}
		}
		if image.ImageKey == "" || image.Volume == "" || filename == "" || !validLFPRotation(image.Rotation) {
			return ErrUnrepresentable
		}
		line := fmt.Sprintf("IM,%s,%s,0,@%s;%s;%s;2,%d\n", image.ImageKey, boundary, image.Volume, strings.ReplaceAll(directory, "/", `\`), filename, image.Rotation)
		if _, err := buffered.WriteString(line); err != nil {
			return fmt.Errorf("write LFP row %d: %w", index+1, err)
		}
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("flush LFP output: %w", err)
	}
	return nil
}

func lfpModelRepresentable(images []ImageRef, index int) bool {
	image := images[index]
	if image.FolderBreak || image.BoxBreak || image.SourcePage != 0 || image.DeclaredPageCount != 0 {
		return false
	}
	if (image.Boundary == "child" || image.Boundary == "document") && !image.DocumentBreak || image.Boundary == "" && image.DocumentBreak {
		return false
	}
	wantPage := 1
	if index > 0 && !image.DocumentBreak {
		wantPage = images[index-1].PageOrdinal + 1
	}
	return image.PageOrdinal == wantPage
}

func lfpBoundary(image ImageRef) (string, error) {
	switch image.Boundary {
	case "document":
		return "D", nil
	case "child":
		return "C", nil
	case "":
		if image.DocumentBreak {
			return "D", nil
		}
		return "", nil
	default:
		return "", ErrUnrepresentable
	}
}

func lfpValueRepresentable(value string) bool {
	return len(value) <= MaxFieldValueBytes && !strings.ContainsAny(value, ",;@\r\n")
}

func validLFPRotation(rotation int) bool {
	return rotation == 0 || rotation == 90 || rotation == 180 || rotation == 270
}
