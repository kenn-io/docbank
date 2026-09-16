// Package mistral provides bounded, stateless document extraction through the
// Mistral OCR API.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. PDF uses a provider-request bound.
// PPTX uses a local slide count. JSON uses one complete top-level value, and
// EML uses one outer RFC 822 message. Each local count must match the
// provider's processed units. Other formats remain unavailable until they have
// a probe-tested unit bound.
package mistral
