package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
	"net/http"
)

func handleWebMailboxChunk(ctx context.Context, conn *websocket.Conn, d Deps, g *gate, begin webUploadMessage) bool {
	hash, hashErr := packstore.ParseHash(begin.ExpectedHash)
	if begin.RequestID == "" || len(begin.RequestID) > 128 || begin.ContainerID == "" || len(begin.ContainerID) > 128 || hashErr != nil || hash.String() != begin.ExpectedHash || begin.ExpectedSize < 1 || begin.ExpectedSize > store.MailboxChunkBytes || begin.ChunkIndex < 0 || begin.ChunkIndex >= store.MailboxMaxChunks {
		return writeWebUploadProblem(ctx, conn, begin.RequestID, NewError(http.StatusUnprocessableEntity, "validation", "invalid mailbox chunk declaration")) == nil
	}
	reader := &webUploadReader{ctx: ctx, conn: conn, requestID: begin.RequestID, inactivity: webUploadInactivity}
	defer reader.close()
	ready := false
	err := g.mutate(func() error {
		if _, err := d.Store.MailboxContainer(ctx, mailboxOwner(d), begin.ContainerID); err != nil {
			return err
		}
		if err := wsjson.Write(ctx, conn, webUploadMessage{Type: "ready", RequestID: begin.RequestID}); err != nil {
			return fmt.Errorf("ready mailbox upload: %w", err)
		}
		ready = true
		return mailboxService(d).UploadChunk(ctx, mailboxOwner(d), begin.ContainerID, begin.ChunkIndex, begin.ExpectedHash, begin.ExpectedSize, reader)
	})
	if err != nil {
		problem := NewError(422, "mailbox_invalid", err.Error())
		if errors.Is(err, errWebUploadCanceled) {
			problem = NewError(499, "canceled", "mailbox upload canceled")
		}
		if writeWebUploadProblem(ctx, conn, begin.RequestID, problem) != nil {
			return false
		}
		return !ready || reader.ended
	}
	return wsjson.Write(ctx, conn, webUploadMessage{Type: "mailbox_chunk_receipt", RequestID: begin.RequestID, ChunkReceipt: &store.MailboxChunk{Index: begin.ChunkIndex, SHA256: begin.ExpectedHash, Size: begin.ExpectedSize}}) == nil
}
