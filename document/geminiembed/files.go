package geminiembed

import (
	"context"
	"errors"
)

func (client *Client) preflightFileRequest(mediaType string, sourceBytes int64) error {
	if sourceBytes > client.profile.MaxRequestBytes {
		return errors.New("gemini embed: raw file upload exceeds request byte capacity")
	}
	payload, err := client.marshalRequest(wirePart{FileData: &wireFileData{
		MIMEType: mediaType, FileURI: maximumProviderFileURI(),
	}})
	clear(payload)
	return err
}

func (client *Client) executeFile(ctx context.Context, file verifiedFile, secret string, receipt *Receipt) (vector []float32, retErr error) {
	uploadURL, err := client.startFileUpload(ctx, file, secret, receipt)
	if err != nil {
		return nil, err
	}
	created, responseID, rawTransferAttempted, err := client.finalizeFileUpload(ctx, uploadURL, file, secret, receipt)
	if err != nil {
		if rawTransferAttempted {
			recordUnconfirmedFileRetention(receipt)
			return nil, errors.Join(err, ErrRemoteRetentionUnconfirmed)
		}
		return nil, err
	}
	deletionConfirmed := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), client.profile.CleanupTimeout)
		defer cancel()
		deleteErr := client.deleteFile(cleanupCtx, created.file.Name, secret, receipt)
		deletionConfirmed = deleteErr == nil
		if deleteErr != nil {
			clear(vector)
			vector = nil
			retErr = errors.Join(retErr, deleteErr)
		}
		if rawTransferAttempted && !deletionConfirmed {
			recordUnconfirmedFileRetention(receipt)
			retErr = errors.Join(retErr, ErrRemoteRetentionUnconfirmed)
		}
	}()
	if !recordProviderResponseID(receipt, responseID) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	active, err := client.waitForActiveFile(ctx, created, file, secret, receipt)
	if err != nil {
		return nil, err
	}
	payload, err := client.marshalRequest(wirePart{FileData: &wireFileData{
		MIMEType: file.metadata.MediaType, FileURI: active.file.URI,
	}})
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	return client.execute(ctx, payload, secret, receipt)
}
