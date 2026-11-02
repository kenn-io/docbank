# spill Instructions

## Scope

`spill/` holds an append-only list of opaque strings in memory up to a byte
limit and in a private temporary file past it. Keep it a container: no
record formats, sorting, or caller policy.

## Invariants

- A list at or under its limit never touches the disk. The limit counts
  each in-memory record's length plus 16 bytes for its string header, so a
  list of short or empty records still moves to disk. `Size` reports payload
  bytes only.
- Create the file only with `safefileio.CreatePrivateTemp` in the caller's
  directory, and remove it on `Close` and when the move to disk fails.
- A failed move to disk leaves the list unchanged. A write failure after the
  move is sticky: every later `Append` and `All` returns it, because the file
  no longer matches the records appended.
- Frame records as a uvarint length followed by the bytes so no byte value is
  reserved.
- Reading checks the file against the record count, payload size, and byte
  count written: the last record must end exactly where the writes did.
  A short, truncated, or inconsistent file is an error, never a shorter list,
  and a length is checked against the payload left and `math.MaxInt` before
  allocating. On 32-bit platforms a file can pass 2 GiB, so the second check
  is what keeps a damaged length from panicking.
- Read with `ReadAt` through a section bounded to the bytes written, so an
  iteration sees the records present when it started and appends during or
  after a read keep writing at the end of the file.
- Keep counts from wrapping on 32-bit platforms: `Append` refuses a record
  once the list holds `math.MaxInt` records, and byte counts widen to
  `int64` before adding.
- `Close` is idempotent and reports flush, close, and remove errors joined.
  A `List` is not safe for concurrent use.
