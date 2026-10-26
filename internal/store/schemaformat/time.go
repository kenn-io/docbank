// Package schemaformat defines representations shared by metadata backends.
package schemaformat

// TimestampLayout keeps fixed-width nanoseconds so UTC timestamps sort as text.
// RFC3339Nano trims trailing zeros and breaks ordering within a second.
const TimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
