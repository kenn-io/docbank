package document

import "slices"

const pendingNoteImage = "Image format support is not cataloged."

var pendingFormats = [...]PendingFormatV1{
	{Label: "BMP", Extensions: []string{"bmp"}, Note: pendingNoteImage},
	{Label: "CSS", Extensions: []string{"css"}, Note: "Source-text support is not cataloged."},
	{Label: "DCX", Extensions: []string{"dcx"}, Note: pendingNoteImage},
	{Label: "DOT", Extensions: []string{"dot"}, Note: "Office document support is not cataloged."},
	{Label: "DOTM", Extensions: []string{"dotm"}, Note: "Office document support is not cataloged."},
	{Label: "DOTX", Extensions: []string{"dotx"}, Note: "Office document support is not cataloged."},
	{Label: "EMLX", Extensions: []string{"emlx"}, Note: "Apple Mail message support is not cataloged."},
	{Label: "EMLXPART", Extensions: []string{"emlxpart"}, Note: "Apple Mail detached-part support is not cataloged."},
	{Label: "HEIC", Extensions: []string{"heic"}, Note: pendingNoteImage},
	{Label: "J2K", Extensions: []string{"j2k"}, Note: pendingNoteImage},
	{Label: "JAVA", Extensions: []string{"java"}, Note: "Source-text support is not cataloged."},
	{Label: "JB2", Extensions: []string{"jb2"}, Note: pendingNoteImage},
	{Label: "JBIG2", Extensions: []string{"jbig2"}, Note: pendingNoteImage},
	{Label: "JFIF", Extensions: []string{"jfif"}, Note: pendingNoteImage},
	{Label: "JP2", Extensions: []string{"jp2"}, Note: pendingNoteImage},
	{Label: "JPC", Extensions: []string{"jpc"}, Note: pendingNoteImage},
	{Label: "JPM", Extensions: []string{"jpm"}, Note: pendingNoteImage},
	{Label: "JPX", Extensions: []string{"jpx"}, Note: pendingNoteImage},
	{Label: "KEY", Extensions: []string{"key"}, Note: "iWork presentation support is not cataloged."},
	{Label: "LEF", Extensions: []string{"l01", "lef"}, Note: "LEF container support is not cataloged."},
	{Label: "ODP", Extensions: []string{"odp"}, Note: "Office presentation support is not cataloged."},
	{Label: "OLM", Extensions: []string{"olm"}, Note: "Outlook for Mac container support is not cataloged."},
	{Label: "OST", Extensions: []string{"ost"}, Note: "Outlook offline store support is not cataloged."},
	{Label: "PAGES", Extensions: []string{"pages"}, Note: "iWork document support is not cataloged."},
	{Label: "PCX", Extensions: []string{"pcx"}, Note: pendingNoteImage},
	{Label: "POT", Extensions: []string{"pot"}, Note: "Office presentation support is not cataloged."},
	{Label: "PPS", Extensions: []string{"pps"}, Note: "Office presentation support is not cataloged."},
	{Label: "PPSX", Extensions: []string{"ppsx"}, Note: "Office presentation support is not cataloged."},
	{Label: "PST", Extensions: []string{"pst"}, Note: "Outlook personal store support is not cataloged."},
	{Label: "TSV", Extensions: []string{"tsv"}, Note: "Delimited-text support is not cataloged."},
	{Label: "WINMAILDAT", Extensions: []string{}, Note: "TNEF container support is not cataloged."},
	{Label: "WPD", Extensions: []string{"wpd"}, Note: "WordPerfect document support is not cataloged."},
	{Label: "XLT", Extensions: []string{"xlt"}, Note: "Office spreadsheet support is not cataloged."},
	{Label: "XLTX", Extensions: []string{"xltx"}, Note: "Office spreadsheet support is not cataloged."},
	{Label: "XLW", Extensions: []string{"xlw"}, Note: "Office spreadsheet support is not cataloged."},
}

// PendingFormats returns recognized formats without catalog support.
// Both the sorted rows and their extension lists are copied.
func PendingFormats() []PendingFormatV1 {
	result := slices.Clone(pendingFormats[:])
	for index := range result {
		result[index].Extensions = slices.Clone(result[index].Extensions)
	}
	return result
}
