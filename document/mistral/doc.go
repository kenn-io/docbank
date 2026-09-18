// Package mistral provides bounded, stateless document extraction through the
// Mistral OCR API.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. PDF uses a provider-request bound
// and PPTX uses a local slide count that must match the provider. When
// PolicyConfig.RenderPDF is set, DOCX uses the configured renderpdf policy and
// the manifest's PDF authority. Mistral counts the generated PDF, checks its
// bytes and pages before HTTP, and sends those exact PDF bytes on every retry.
// Text formats use provider-response enforcement: returned pages must be
// positive, agree with pages_processed, and fit MaxUnits. Input byte limits
// apply before upload; text lines and records are not counted. A rejected text
// response may still incur provider charges, so text authority is not a
// pre-upload page or spending bound.
//
// XLSX remains unauthorized: a worksheet count does not establish the number
// of billable pages.
package mistral
