package loadfile

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxLFPLineBytes = 4*MaxFieldValueBytes + 128

// ScanLFP reads supported IPRO image records and ignores comment records.
func ScanLFP(ctx context.Context, source io.Reader, profile Profile, visit func(ImageRef) error) error {
	if profile.ID != "lfp-ipro-v1" || profile.Field != ',' {
		return ErrInvalidProfile
	}
	decoder, err := Decoder(profile.Encoding)
	if err != nil {
		return err
	}
	imageCount := 0
	pageOrdinal := 0
	rowOrdinal := 0
	scanner := bufio.NewScanner(decoder(source))
	scanner.Buffer(make([]byte, validationReadSize), maxLFPLineBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		rowOrdinal++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, "##") {
			continue
		}
		values := strings.Split(line, ",")
		command := ""
		if len(values) > 0 {
			command = values[0]
		}
		if command != "IM" {
			return fmt.Errorf("%w: unsupported LFP command %q on row %d", ErrUnrepresentable, command, rowOrdinal)
		}
		if imageCount == 1_000_000 {
			return ErrLoadfileLimit
		}
		if len(values) != 6 {
			return fmt.Errorf("%w: LFP IM row %d has %d fields", ErrMalformedInput, rowOrdinal, len(values))
		}
		for _, value := range values {
			if len(value) > MaxFieldValueBytes {
				return fmt.Errorf("%w: LFP row %d field exceeds %d bytes", ErrMalformedInput, rowOrdinal, MaxFieldValueBytes)
			}
		}
		if values[3] != "0" {
			return fmt.Errorf("%w: nonzero LFP offset on row %d", ErrUnrepresentable, rowOrdinal)
		}
		group := strings.Split(values[4], ";")
		if len(group) != 4 || !strings.HasPrefix(group[0], "@") || len(group[0]) == 1 || group[2] == "" {
			return fmt.Errorf("%w: malformed LFP image group on row %d", ErrMalformedInput, rowOrdinal)
		}
		if group[3] != "2" {
			return fmt.Errorf("%w: unsupported LFP image type %q", ErrUnrepresentable, group[3])
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
			return fmt.Errorf("%w: unsupported LFP boundary %q", ErrUnrepresentable, values[2])
		}
		rotation, parseErr := strconv.Atoi(values[5])
		if parseErr != nil || !validLFPRotation(rotation) {
			return fmt.Errorf("%w: invalid LFP rotation on row %d", ErrMalformedInput, rowOrdinal)
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
		if err := visit(ImageRef{
			ImageKey: values[1], Volume: strings.TrimPrefix(group[0], "@"), RelPath: relPath,
			DocumentBreak: documentBreak, PageOrdinal: pageOrdinal, Boundary: boundary, Rotation: rotation,
		}); err != nil {
			return err
		}
		imageCount++
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("%w: LFP row exceeds %d bytes", ErrMalformedInput, maxLFPLineBytes)
		}
		return fmt.Errorf("read LFP input: %w", err)
	}
	return nil
}

func validLFPRotation(rotation int) bool {
	return rotation == 0 || rotation == 90 || rotation == 180 || rotation == 270
}
