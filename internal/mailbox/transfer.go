package mailbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/docbank/internal/store"
)

var ErrRejected = errors.New("email occurrence rejected")

// Transfer accepts an explicitly identified EML from any registered archive.
// It does not discover or connect to another application's data or accounts.
func (s *Service) Transfer(ctx context.Context, owner string, request store.MailboxTransferRequest, input io.Reader) (_ store.MailboxTransferReceipt, retErr error) {
	run, err := s.Store.BeginIngest(ctx, "mailbox", "Explicit EML transfer")
	if err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	if request.Size < 1 || request.Size > 128<<20 {
		return store.MailboxTransferReceipt{}, store.ErrMailboxLimit
	}
	source, err := emailmime.NewSourceSpool(s.Spool)
	if err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(source, h), io.LimitReader(contextReader{ctx, input}, request.Size+1))
	if err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	if n != request.Size || hex.EncodeToString(h.Sum(nil)) != request.SHA256 {
		return store.MailboxTransferReceipt{}, store.ErrMailboxConflict
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	return s.transferSource(ctx, owner, run, request, source, nil, nil)
}

type occurrenceCommit struct {
	job        store.MailboxJob
	occurrence store.MailboxOccurrence
}

// transferSource borrows the scanner's verified source until publication ends.
// The decoder owns its independent verified inventory spool.
func (s *Service) transferSource(ctx context.Context, owner string, run store.IngestRun, request store.MailboxTransferRequest, source *emailmime.SourceSpool, location *store.MailboxLocation, commit *occurrenceCommit) (_ store.MailboxTransferReceipt, retErr error) {
	decoded, err := emailmime.Decode(ctx, request.SHA256, request.Size, source, s.Spool)
	if decoded != nil {
		defer func() { retErr = errors.Join(retErr, decoded.Close()) }()
	}
	if err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	if decoded.Evidence.Inventory == nil || decoded.Evidence.Inventory.State != document.EmailInventoryComplete {
		return store.MailboxTransferReceipt{}, fmt.Errorf("%w: malformed or over-limit MIME inventory", ErrRejected)
	}
	if location != nil {
		labels, err := messageLabels(ctx, decoded)
		if err != nil {
			return store.MailboxTransferReceipt{}, err
		}
		location.Labels = labels
		if commit != nil {
			commit.occurrence.Location = *location
		}
	}
	canonical, _, err := document.MarshalEmailV1(decoded.Evidence)
	if err != nil {
		return store.MailboxTransferReceipt{}, err
	}
	p := store.MailboxTransferPublication{Owner: owner, Run: run, Request: request, Email: store.EmailPublication{CanonicalJSON: canonical, Artifacts: []store.EmailPartArtifactRecord{}}, Location: location}
	var receipt store.MailboxTransferReceipt
	err = s.mutate(ctx, func() error {
		return s.Blobs.WithMutation(ctx, func() error {
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := s.stageBlob(ctx, source, request.SHA256, request.Size); err != nil {
				return err
			}
			for _, a := range decoded.Artifacts() {
				reader, err := decoded.OpenArtifact(ctx, a.PartPath, string(a.Reference.Role))
				if err != nil {
					return err
				}
				stageErr := s.stageBlob(ctx, reader, a.Reference.SHA256, a.Reference.Size)
				if err = errors.Join(stageErr, reader.Close()); err != nil {
					return err
				}
				p.Email.Artifacts = append(p.Email.Artifacts, store.EmailPartArtifactRecord{PartPath: a.PartPath, Role: string(a.Reference.Role), BlobSHA256: a.Reference.SHA256, Size: a.Reference.Size})
			}
			if commit != nil {
				return s.Store.CommitMailboxOccurrence(ctx, commit.job.ID, commit.job.Claim, commit.occurrence, &p)
			}
			var err error
			receipt, err = s.Store.PublishMailboxTransfer(ctx, p)
			return err
		})
	})
	return receipt, err
}
func (s *Service) stageBlob(ctx context.Context, r io.Reader, hash string, size int64) error {
	wr, err := s.Blobs.WriteDetailedContext(ctx, io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	enc, err := wr.EncodingName()
	if err != nil {
		return err
	}
	if err = s.Store.RecordBlob(ctx, wr.Hash, wr.Size, store.BlobPhysical{Encoding: enc, StoredBytes: wr.StoredSize, PackEligible: wr.PackEligible, Created: wr.Created}); err != nil {
		return err
	}
	if wr.Hash != hash || wr.Size != size {
		return store.ErrMailboxConflict
	}
	return nil
}

func messageLabels(ctx context.Context, decoded *emailmime.Result) ([]string, error) {
	labels := []string{}
	part := decoded.Evidence.Inventory.Parts[0]
	if part.HeaderBlock == nil {
		return labels, nil
	}
	reader, err := decoded.OpenArtifact(ctx, part.Path, string(document.EmailArtifactRawHeaders))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err = errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	for _, header := range part.Headers {
		if header.Name == nil || *header.Name != "x-gmail-labels" {
			continue
		}
		value, err := emailmime.HeaderValue(raw, header)
		if err != nil {
			return nil, err
		}
		for label := range strings.SplitSeq(value, ",") {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			if len(label) > 256 || len(labels) >= 100 {
				return nil, fmt.Errorf("%w: source label limit", ErrRejected)
			}
			labels = append(labels, label)
		}
	}
	return labels, nil
}
