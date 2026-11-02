// Package spill holds an append-only list of strings in memory while it is
// small and moves it to a private temporary file once it grows past a byte
// limit, so a caller can collect a list of unknown size without holding all
// of it in memory.
//
// New takes the directory for the file, a file name pattern, and the limit.
// Records stay in memory while their lengths plus 16 bytes per record, the
// size of a string header, total at most the limit, and no file is created.
// The Append that would pass the limit creates the file with
// safefileio.CreatePrivateTemp, writes the buffered records to it, drops
// them from memory, and appends that record and every later one through a
// buffered writer. After the switch, memory is bounded by the writer buffer,
// not the list size.
//
// Records are opaque: any bytes are allowed, including NUL, newlines, and
// the empty string. The file stores each record as a uvarint length followed
// by its bytes.
//
// All yields the records in insertion order, reading the file after flushing
// buffered writes. A read error or a corrupt or truncated file is yielded as
// an error, never as a shorter list. Append may be called after or during a
// read; an iteration yields the records appended before it started.
//
// Close removes the file. It is safe to call more than once; calls after the
// first return nil. A List is not safe for concurrent use.
package spill
