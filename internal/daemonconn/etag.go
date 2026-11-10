package daemonconn

import (
	"errors"
	"fmt"
)

func validateRevisionETag(etag string, revision int64) error {
	if etag == "" {
		return errors.New("response is missing ETag")
	}
	want := revisionIfMatch(revision)
	if etag != want {
		return fmt.Errorf("response ETag %q, expected %q", etag, want)
	}
	return nil
}
