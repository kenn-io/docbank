// Package s3store provides an optional S3-compatible packstore backend.
package s3store

import (
	"fmt"
	"path"
	"strings"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const ownershipName = ".kit-store.json"

type keyspace struct {
	prefix string
}

func newKeyspace(prefix string) (keyspace, error) {
	if prefix == "" {
		return keyspace{}, nil
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, `\`) ||
		strings.Contains(prefix, "//") || path.Clean(prefix) != prefix ||
		prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") {
		return keyspace{}, fmt.Errorf("s3store: prefix %q is not canonical", prefix)
	}
	return keyspace{prefix: prefix}, nil
}

func (k keyspace) join(parts ...string) string {
	name := path.Join(parts...)
	if name == "." {
		name = ""
	}
	if k.prefix == "" {
		return name
	}
	if name == "" {
		return k.prefix + "/"
	}
	return k.prefix + "/" + name
}

func (k keyspace) ownership() string { return k.join(ownershipName) }

func (k keyspace) loose(hash packstore.Hash, encoding packstore.LooseEncoding) string {
	name := hash.String()
	if encoding == packstore.LooseEncodingZstd {
		name += ".zst"
	}
	return k.join("loose", hash.String()[:2], name)
}

func (k keyspace) pack(packID string) string {
	return k.join("packs", packID[:2], packID+".pack")
}

func (k keyspace) staging(epoch string, operationID string, part string) string {
	return k.join("staging", epoch, operationID, part)
}

func (k keyspace) relative(key string) (string, bool) {
	if k.prefix == "" {
		return key, key != ""
	}
	prefix := k.prefix + "/"
	return strings.TrimPrefix(key, prefix), strings.HasPrefix(key, prefix)
}

func (k keyspace) objectRef(key string) (packstore.ObjectRef, bool) {
	relative, ok := k.relative(key)
	if !ok {
		return packstore.ObjectRef{}, false
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 3 {
		return packstore.ObjectRef{}, false
	}
	switch parts[0] {
	case "loose":
		name := parts[2]
		encoding := packstore.LooseEncodingRaw
		if trimmed, found := strings.CutSuffix(name, ".zst"); found {
			name = trimmed
			encoding = packstore.LooseEncodingZstd
		}
		hash, err := packstore.ParseHash(name)
		if err != nil || parts[1] != name[:2] {
			return packstore.ObjectRef{}, false
		}
		return packstore.ObjectRef{LooseHash: hash, LooseEncoding: encoding}, true
	case "packs":
		name := strings.TrimSuffix(parts[2], ".pack")
		if !strings.HasSuffix(parts[2], ".pack") || !pack.IsValidPackID(name) ||
			parts[1] != name[:2] {
			return packstore.ObjectRef{}, false
		}
		return packstore.ObjectRef{PackID: name}, true
	default:
		return packstore.ObjectRef{}, false
	}
}
