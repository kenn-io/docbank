// Package mistral provides bounded, stateless document extraction through the
// Mistral OCR API.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. The initial fixture contract can
// authorize PDF only; other formats remain unavailable until they have a
// probe-tested unit bound.
package mistral
