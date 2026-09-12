package docbank

import (
	"context"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
	"io"
)

// MailboxTransferRequest identifies one explicitly exported message. It never
// grants access to another application's accounts or files.
type MailboxTransferRequest = store.MailboxTransferRequest
type MailboxTransferReceipt = store.MailboxTransferReceipt
type MailboxLocation = store.MailboxLocation

var ErrMailboxConflict = store.ErrMailboxConflict
var ErrMailboxLimit = store.ErrMailboxLimit
var ErrMailboxInvalid = store.ErrMailboxInvalid

// RegisterMailboxArchive registers a durable, application-independent source
// identity. Its mappings have no TTL and are retained by portable backups.
func (v *Vault) RegisterMailboxArchive(ctx context.Context, id, description string) error {
	if err := v.begin(); err != nil {
		return err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return v.metadata.RegisterMailboxArchive(ctx, store.MailboxArchive{ID: id, Owner: "vault:" + v.metadata.VaultID(), Description: description})
}

// TransferEML verifies bytes and atomically publishes message, known decoded
// attachments and receipt. A retry never resurrects a trashed target; changed
// source bytes require the original target's expected revision.
func (v *Vault) TransferEML(ctx context.Context, request MailboxTransferRequest, source io.Reader) (MailboxTransferReceipt, error) {
	if err := v.begin(); err != nil {
		return MailboxTransferReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	service := mailbox.Service{Store: v.metadata, Blobs: v.blobs, Spool: v.emailSpoolParent}
	return service.Transfer(ctx, "vault:"+v.metadata.VaultID(), request, source)
}
