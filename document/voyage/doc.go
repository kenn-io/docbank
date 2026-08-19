// Package voyage provides bounded, stateless multimodal embedding of images
// and video through the Voyage AI API.
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
