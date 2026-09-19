// Package renderpdf converts byte-verified office sources to bounded PDFs
// through a pinned LibreOffice installation. It supports DOCX, DOC, ODT, RTF,
// PPT, XLS, ODS, and XLSX through eight profiles. Writer profiles normalize to
// FODT, presentation profiles to FODP, and spreadsheet profiles to FODS.
//
// Conversion first normalizes the source to flat ODF inside the provider
// sandbox. A bounded ODF scan then decides whether those exact bytes may reach
// the PDF stage. The result owns the counted PDF and a receipt that links the
// source, normalized document, and PDF identities. It grants no upload
// authority.
package renderpdf
