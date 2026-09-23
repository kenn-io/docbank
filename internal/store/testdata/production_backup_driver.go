// This separate process lets Store's integration fixture exercise backupapp
// without making the store test package import one of its own consumers.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func main() {
	if len(os.Args) != 4 {
		panic("usage: production_backup_driver create|restore source-or-repo repo-or-target")
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(action, source, target string) error {
	ctx := context.Background()
	switch action {
	case "create":
		metadata, err := store.Open(filepath.Join(source, "docbank.db"))
		if err != nil {
			return err
		}
		defer metadata.Close()
		physical, err := blob.New(store.NewPackCatalog(metadata), filepath.Join(source, "blobs"))
		if err != nil {
			return err
		}
		defer physical.Close()
		repo, err := backup.Init(target)
		if err != nil {
			return err
		}
		_, err = backupapp.Create(ctx, repo, "synthetic-test", metadata, physical, backup.CreateOptions{Jobs: 1})
		return err
	case "restore":
		repo, err := backup.Open(source)
		if err != nil {
			return err
		}
		_, err = backupapp.Restore(ctx, repo, "synthetic-test", backup.RestoreOptions{TargetDir: target, Jobs: 1})
		return err
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}
