// Package embedmodel describes a vector space, an indexed-input recipe, and
// the content those inputs carry.
//
// Vector-space identity and input-recipe identity answer different
// questions. A chunk-window change rebuilds inputs inside the same comparison
// space. An API key, timeout, or batch size changes neither. The lexical
// analyzer identity lives in search/lexical.
//
// A consumer that already has embedded generations lists their fingerprints
// in Descriptor.Legacy. Matches keeps those generations current, so adopting
// this package does not force a re-embed; new generations use kit's form.
//
// Content is not a bare list of strings. Text is the first encoded form, and
// an empty Kind means text.
// Image and file content remains a valid description so callers can keep
// those paths. The shared text client does not encode a top-level image or
// file, even when Text is set, or a part of those kinds.
// Source spans are coordinates into the caller's source, not into the
// formatted request.
package embedmodel
