package openssh

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"path/filepath"
	"strconv"
	"strings"
)

const controlDigestBytes = 16

var controlNameEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// RouteIdentity binds a caller scope to every logical route target and its
// normalized effective OpenSSH configuration.
func RouteIdentity(scope string, route Route) string {
	digest := sha256.New()
	writeIdentityValue(digest, scope)
	for _, target := range route {
		writeIdentityValue(digest, target.Target.User)
		writeIdentityValue(digest, target.Target.Hostname)
		writeIdentityValue(digest, strconv.Itoa(target.Target.Port))
		writeIdentityValue(digest, target.Config.User)
		writeIdentityValue(digest, target.Config.Hostname)
		writeIdentityValue(digest, strconv.Itoa(target.Config.Port))
		writeIdentityValue(digest, target.Config.HostKeyAlias)
		writeIdentityValue(digest, target.Config.StrictHostKeyChecking)
		writeIdentityValue(digest, target.Config.ProxyJump)
		writeIdentityValue(digest, target.Config.ProxyCommand)
		for _, option := range target.Config.CanonicalOptions() {
			writeIdentityValue(digest, option)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeIdentityValue(digest hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write([]byte(value))
}

// ControlName returns a short deterministic name safe for a socket filename.
func ControlName(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return "c-" + controlNameEncoding.EncodeToString(digest[:controlDigestBytes])
}

func controlNameForTarget(identity string, target Target) string {
	digest := sha256.New()
	writeIdentityValue(digest, identity)
	writeIdentityValue(digest, target.User)
	writeIdentityValue(digest, target.Hostname)
	writeIdentityValue(digest, strconv.Itoa(target.Port))
	sum := digest.Sum(nil)
	return "c-" + controlNameEncoding.EncodeToString(sum[:controlDigestBytes])
}

// ControlPath joins a control name to directory and enforces the caller's
// Unix-domain socket byte limit. A zero limit disables the length check.
func ControlPath(directory, name string, maximumBytes int) (string, error) {
	if directory == "" {
		return "", &PathError{Reason: "empty control directory"}
	}
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\\`) {
		return "", &PathError{Reason: "invalid control name"}
	}
	path := filepath.Join(directory, name+".sock")
	if maximumBytes > 0 && len([]byte(path)) > maximumBytes {
		return "", &PathError{Path: path, MaximumBytes: maximumBytes, Reason: "control path is too long"}
	}
	return path, nil
}
