// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint only when the app has no configured endpoint. It prefers Ollama
// on its usual loopback port when nomic-embed-text:latest is installed.
// The returned /v1 base URL leaves model names and input recipes to the caller.
// Obtain download consent before supplying an executable when model assets may
// be fetched. Allow time for the first model download with a context deadline.
// Without a caller deadline, startup waits at most two minutes. A detached
// startup continues loading after cancellation, and later callers can join it.
// Startup diagnostics live in kennembed.log in the shared data directory.
package embedlocal
