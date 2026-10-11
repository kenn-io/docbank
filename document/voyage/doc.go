// Package voyage provides bounded, stateless multimodal embedding of images
// and video, plus retained rendition text, through the Voyage AI API.
// Text retrieval requires an operator-managed deployment epoch matching the
// descriptor revision. Unpinned hosted aliases remain export-only.
//
// Uploads fail closed unless an operator has run the authenticated capability
// probe and supplied its validated manifest. Each media format, animated
// image support, video, and query mode is authorized separately by recorded
// probe evidence rather than by assumption.
//
// The package detects and bounds media through go.kenn.io/docbank/document/media
// and never persists media bytes, vectors, or provider responses. Consent,
// spending limits, orchestration, vector storage, and search remain the
// importing application's responsibility.
package voyage
