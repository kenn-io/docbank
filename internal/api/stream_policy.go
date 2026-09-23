package api

import (
	"context"
	"hash"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// streamBodyAuthorized checks at the last point before response headers or
// bytes are published. A revoked protected source receives no identity header.
func streamBodyAuthorized(ctx huma.Context, deps Deps, sourceID string) bool {
	if _, err := authorizeRequest(ctx.Context(), deps, OperationRead, []string{sourceID}, true, true); err != nil {
		ctx.SetStatus(http.StatusNotFound)
		return false
	}
	return true
}

type sourceGrantWriter struct {
	ctx      context.Context
	deps     Deps
	sourceID string
	dest     io.Writer
}

func (w sourceGrantWriter) Write(p []byte) (int, error) {
	if _, err := authorizeRequest(w.ctx, w.deps, OperationRead, []string{w.sourceID}, true, true); err != nil {
		return 0, err
	}
	return w.dest.Write(p)
}

// copyAuthorizedStream rechecks before every chunk, including after a blocked
// source read. A mid-stream revocation stops all subsequent bytes.
func copyAuthorizedStream(ctx context.Context, deps Deps, sourceID string,
	dest io.Writer, source io.Reader, digest hash.Hash,
) error {
	_, err := io.Copy(sourceGrantWriter{ctx: ctx, deps: deps, sourceID: sourceID, dest: dest},
		io.TeeReader(source, digest))
	return err
}
