// Package embedconfig holds reusable embedding settings.
//
// Applications own files, flags, and persistence. This package owns the value
// types, defaults, and validation those applications were copying, and
// Embedder, the standard configuration-file shape for one embedding endpoint,
// so every application reads the same keys.
//
// The types stay separate on purpose. Model, role, and input settings can
// change vectors or force a rebuild. Batch size and timeouts do not.
// embedmodel.Descriptor combines them and computes the identities.
//
// Dimensions and truncation have no default. Batch size defaults to 32
// items, which every common embedding server and provider accepts, and the
// transport timeout defaults to 30 seconds. Deployments that need other
// values, such as 128 items or a 45 second timeout, set them explicitly.
package embedconfig
