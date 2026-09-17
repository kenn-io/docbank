// Package mistral provides bounded, stateless document extraction through the
// Mistral OCR API.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. PDF uses a provider-request bound
// and PPTX uses a local slide count that must match the provider. TXT,
// Markdown, Go, Python, JavaScript, RST, and LaTeX count lines. CSV and JSONL
// count records. JSON and XML count one document, YAML counts documents, and
// EML and MSG count one outer message. Text source counts must fit MaxUnits
// before upload. Provider pages must remain positive and within that limit, but
// may differ from a text source count.
package mistral
