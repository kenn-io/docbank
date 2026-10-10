package loadfile

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// WriteLFP emits the supported IPRO IM subset in page order. SourcePage,
// declared counts, and OPT-only flags must be retained in the package page
// map because LFP has no fields for them. As with WriteDAT, callers publish
// through a temporary file because destination I/O can fail mid-write.
func WriteLFP(destination io.Writer, images []ImageRef, profile Profile) error {
	if profile.ID != "lfp-ipro-v1" || profile.Field != ',' {
		return ErrInvalidProfile
	}
	if len(images) > 1_000_000 {
		return ErrLoadfileLimit
	}
	return writeLoadfile(destination, profile.Encoding, func(output io.Writer) error {
		pageOrdinal := 0
		for index, image := range images {
			if image.DocumentBreak || index == 0 {
				pageOrdinal = 1
			} else {
				pageOrdinal++
			}
			if image.PageOrdinal != pageOrdinal || !validLFPRotation(image.Rotation) {
				return fmt.Errorf("%w: LFP page %d has inconsistent ordinal or rotation", ErrUnrepresentable, index+1)
			}
			flag := ""
			switch {
			case image.DocumentBreak && image.Boundary == "document":
				flag = "D"
			case image.DocumentBreak && image.Boundary == "child":
				flag = "C"
			case !image.DocumentBreak && image.Boundary == "":
			default:
				return fmt.Errorf("%w: LFP page %d has inconsistent boundary", ErrUnrepresentable, index+1)
			}
			path, err := portablePackagePath(image.RelPath)
			if err != nil || path != image.RelPath {
				return fmt.Errorf("%w: LFP page %d has unsafe path", ErrUnrepresentable, index+1)
			}
			directory, file := "", path
			if lastSlash := strings.LastIndexByte(path, '/'); lastSlash >= 0 {
				directory = strings.ReplaceAll(path[:lastSlash], "/", `\`)
				file = path[lastSlash+1:]
			}
			group := "@" + image.Volume + ";" + directory + ";" + file + ";2"
			if len(group) > MaxFieldValueBytes {
				return fmt.Errorf("%w: LFP page %d group exceeds field limit", ErrUnrepresentable, index+1)
			}
			for _, value := range []string{image.ImageKey, image.Volume, image.RelPath} {
				if value == "" || !utf8.ValidString(value) || len(value) > MaxFieldValueBytes || strings.ContainsAny(value, ",;@\r\n") {
					return fmt.Errorf("%w: LFP page %d contains an ambiguous field", ErrUnrepresentable, index+1)
				}
			}
			line := "IM," + image.ImageKey + "," + flag + ",0," + group + "," + strconv.Itoa(image.Rotation) + "\r\n"
			if len(line) > maxLFPLineBytes {
				return ErrLoadfileLimit
			}
			if _, err := io.WriteString(output, line); err != nil {
				return err
			}
		}
		return nil
	})
}
