package store

import "strings"

const (
	columnBlobHash     = "blob_hash"
	columnSourceSHA256 = "source_sha256"
)

// blobReference names one catalog column whose rows keep a blob reachable.
type blobReference struct {
	table  string
	column string
}

// blobRootReferences is the single definition of "something still needs
// these bytes" shared by garbage collection, version pruning, derivative
// purge, verification, and backup capture. A blob referenced by any of these
// rows is reachable; a blob referenced by none is a collection candidate.
// Visual preview sources are omitted because the Go writer binds them to the
// same hash as their content version, which is already listed.
var blobRootReferences = []blobReference{
	{table: "content_versions", column: columnBlobHash},
	{table: "rendition_builds", column: columnSourceSHA256},
	{table: "rendition_jobs", column: columnSourceSHA256},
	{table: "rendition_artifacts", column: columnBlobHash},
	{table: "visual_preview_generations", column: "output_blob_hash"},
}

// blobGCHolds keep a blob out of ordinary garbage collection without making
// it reachable: rendition bytes staged before a manifest owns them, and
// exact-erasure targets that the derivative purge path retires itself.
var blobGCHolds = []blobReference{
	{table: "rendition_blob_staging", column: columnBlobHash},
	{table: "derivative_blob_purge_pending", column: columnBlobHash},
}

// blobUnreferencedSQL returns an AND-joined predicate that is true when no
// listed reference names the blob identified by hashExpr.
func blobUnreferencedSQL(hashExpr string, references ...[]blobReference) string {
	var clauses []string
	for _, group := range references {
		for _, reference := range group {
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM "+reference.table+
				" r WHERE r."+reference.column+" = "+hashExpr+")")
		}
	}
	return strings.Join(clauses, "\n\t\t  AND ")
}

// blobReferencedSQL returns an OR-joined predicate that is true when any
// listed reference names the blob identified by hashExpr.
func blobReferencedSQL(hashExpr string, references ...[]blobReference) string {
	var clauses []string
	for _, group := range references {
		for _, reference := range group {
			clauses = append(clauses, "EXISTS (SELECT 1 FROM "+reference.table+
				" r WHERE r."+reference.column+" = "+hashExpr+")")
		}
	}
	return strings.Join(clauses, "\n\t\t   OR ")
}

// blobReferenceRowsSQL returns a UNION ALL of every listed reference as a
// single blob_hash column, one row per referencing catalog row, for callers
// that count references rather than test reachability.
func blobReferenceRowsSQL(references ...[]blobReference) string {
	var selects []string
	for _, group := range references {
		for _, reference := range group {
			selects = append(selects, "SELECT "+reference.column+" AS blob_hash FROM "+
				reference.table+" WHERE "+reference.column+" IS NOT NULL")
		}
	}
	return strings.Join(selects, "\n\t\t\tUNION ALL\n\t\t\t")
}

// blobReferenceSetSQL returns a distinct set of non-NULL blob hashes from the
// listed references, preserving the order of the reference list.
func blobReferenceSetSQL(references []blobReference) string {
	selects := make([]string, 0, len(references))
	for _, reference := range references {
		selects = append(selects, "SELECT "+reference.column+" FROM "+reference.table+
			" WHERE "+reference.column+" IS NOT NULL")
	}
	return strings.Join(selects, "\n\tUNION\n\t")
}

// BackupBlobAuthorityCTE renders the complete blob closure for portable
// backup. GC-only holds intentionally remain outside this authority.
func BackupBlobAuthorityCTE() string {
	return "WITH backup_authorized_blobs(hash) AS (\n\t" +
		blobReferenceSetSQL(blobRootReferences) + "\n)\n"
}
