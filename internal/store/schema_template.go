package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/atomicfile"

	docsqlite "go.kenn.io/docbank/sqlite"
)

// The default driver's empty current-schema database, built once per process.
// Opening an existing vault compares its columns with the template's, and a
// new vault starts as a copy of the template's file. Applying the schema takes
// hundreds of statements, which made each open take tens of milliseconds.
// Only the default driver is cached: other drivers, such as test drivers that
// change the schema, may each produce a different database.
var schemaTemplate struct {
	mu     sync.Mutex
	schema string
	built  *builtSchema
}

// defaultSchemaTemplate returns the template for the current schemaSQL. A
// failed build is not kept, so the next open tries again.
func defaultSchemaTemplate(ctx context.Context, scratchDir string) (*builtSchema, error) {
	schemaTemplate.mu.Lock()
	defer schemaTemplate.mu.Unlock()
	if schemaTemplate.built == nil || schemaTemplate.schema != schemaSQL {
		built, err := buildCurrentSchema(ctx, DefaultSQLiteDriver(), scratchDir, true)
		if err != nil {
			return nil, err
		}
		schemaTemplate.schema, schemaTemplate.built = schemaSQL, &built
	}
	return schemaTemplate.built, nil
}

// createFromSchemaTemplate writes the schema template to path when no file
// exists there, so bootstrap finds every schema object in place. Another
// opener that creates path first wins, and this open uses its database.
//
// The template only saves time. When it cannot be built or written, SQLite
// creates the database and bootstrap applies the schema in its transaction,
// which reports a schema error and rolls back what it applied.
func createFromSchemaTemplate(ctx context.Context, path string, driver docsqlite.Driver) {
	if driver != DefaultSQLiteDriver() {
		return
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	template, err := defaultSchemaTemplate(ctx, filepath.Dir(path))
	if err != nil {
		return
	}
	// SQLite creates databases with mode 0644 less the umask. WriteNew never
	// replaces a file and removes its staging file when it fails.
	_ = atomicfile.WriteNew(path, template.image, atomicfile.WithCreatePerm(0o644))
}
