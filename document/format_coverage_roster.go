package document

import "slices"

const (
	pendingOwnerOffice = "DB-42a"
	pendingOwnerImage  = "DB-42e"
	pendingNoteImage   = "Image catalog support is owned by DB-42e."
)

var pendingFormats = [...]PendingFormatV1{
	{Label: "BMP", Extensions: []string{"bmp"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "CSS", Extensions: []string{"css"}, OwnerSlice: "DB-42g", Note: "Source-text catalog support is owned by DB-42g."},
	{Label: "DCX", Extensions: []string{"dcx"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "DOT", Extensions: []string{"dot"}, OwnerSlice: pendingOwnerOffice, Note: "Office document catalog support is owned by DB-42a."},
	{Label: "DOTM", Extensions: []string{"dotm"}, OwnerSlice: pendingOwnerOffice, Note: "Office document catalog support is owned by DB-42a."},
	{Label: "DOTX", Extensions: []string{"dotx"}, OwnerSlice: pendingOwnerOffice, Note: "Office document catalog support is owned by DB-42a."},
	{Label: "EMLX", Extensions: []string{"emlx"}, OwnerSlice: "DB-43b", Note: "Apple Mail message support is owned by DB-43b."},
	{Label: "EMLXPART", Extensions: []string{"emlxpart"}, OwnerSlice: "DB-43b", Note: "Apple Mail detached-part support is owned by DB-43b."},
	{Label: "HEIC", Extensions: []string{"heic"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "J2K", Extensions: []string{"j2k"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JAVA", Extensions: []string{"java"}, OwnerSlice: "DB-42g", Note: "Source-text catalog support is owned by DB-42g."},
	{Label: "JB2", Extensions: []string{"jb2"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JBIG2", Extensions: []string{"jbig2"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JFIF", Extensions: []string{"jfif"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JP2", Extensions: []string{"jp2"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JPC", Extensions: []string{"jpc"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JPM", Extensions: []string{"jpm"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "JPX", Extensions: []string{"jpx"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "KEY", Extensions: []string{"key"}, OwnerSlice: "DB-42c", Note: "iWork presentation catalog support is owned by DB-42c."},
	{Label: "LEF", Extensions: []string{"l01", "lef"}, OwnerSlice: "DB-40c", Note: "LEF container catalog support is owned by DB-40c."},
	{Label: "ODP", Extensions: []string{"odp"}, OwnerSlice: pendingOwnerOffice, Note: "Office presentation catalog support is owned by DB-42a."},
	{Label: "OLM", Extensions: []string{"olm"}, OwnerSlice: "DB-43d", Note: "Outlook for Mac container support is owned by DB-43d."},
	{Label: "OST", Extensions: []string{"ost"}, OwnerSlice: "DB-43c", Note: "Outlook offline store support is owned by DB-43c."},
	{Label: "PAGES", Extensions: []string{"pages"}, OwnerSlice: "DB-42c", Note: "iWork document catalog support is owned by DB-42c."},
	{Label: "PCX", Extensions: []string{"pcx"}, OwnerSlice: pendingOwnerImage, Note: pendingNoteImage},
	{Label: "POT", Extensions: []string{"pot"}, OwnerSlice: pendingOwnerOffice, Note: "Office presentation catalog support is owned by DB-42a."},
	{Label: "PPS", Extensions: []string{"pps"}, OwnerSlice: pendingOwnerOffice, Note: "Office presentation catalog support is owned by DB-42a."},
	{Label: "PPSX", Extensions: []string{"ppsx"}, OwnerSlice: pendingOwnerOffice, Note: "Office presentation catalog support is owned by DB-42a."},
	{Label: "PST", Extensions: []string{"pst"}, OwnerSlice: "DB-43c", Note: "Outlook personal store support is owned by DB-43c."},
	{Label: "TSV", Extensions: []string{"tsv"}, OwnerSlice: "DB-42g", Note: "Delimited-text catalog support is owned by DB-42g."},
	{Label: "WINMAILDAT", Extensions: []string{}, OwnerSlice: "DB-43a", Note: "TNEF container support is owned by DB-43a."},
	{Label: "WPD", Extensions: []string{"wpd"}, OwnerSlice: "DB-42b", Note: "WordPerfect document support is owned by DB-42b."},
	{Label: "XLT", Extensions: []string{"xlt"}, OwnerSlice: pendingOwnerOffice, Note: "Office spreadsheet catalog support is owned by DB-42a."},
	{Label: "XLTX", Extensions: []string{"xltx"}, OwnerSlice: pendingOwnerOffice, Note: "Office spreadsheet catalog support is owned by DB-42a."},
	{Label: "XLW", Extensions: []string{"xlw"}, OwnerSlice: pendingOwnerOffice, Note: "Office spreadsheet catalog support is owned by DB-42a."},
}

// PendingFormats returns the sorted research roster whose catalog work belongs
// to later slices. Both the rows and their extension lists are copied.
func PendingFormats() []PendingFormatV1 {
	result := slices.Clone(pendingFormats[:])
	for index := range result {
		result[index].Extensions = slices.Clone(result[index].Extensions)
	}
	return result
}
