// Package mistral provides bounded, stateless document extraction through the
// Mistral OCR API.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. PDF uses a provider-request bound
// and PPTX uses a local slide count that must match the provider. Text formats
// use provider-response enforcement: returned pages must be positive, agree
// with pages_processed, and fit MaxUnits. Input byte limits apply before upload;
// text lines and records are not counted. A rejected response may still incur
// provider charges, so this is not a pre-upload page or spending bound.
//
// XLSX remains unauthorized: a worksheet count does not establish the number
// of billable pages.
package mistral
