package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const probeCleanupTimeout = 10 * time.Second

// Inventory returns one bounded, paginated namespace page. Only canonical
// Kit-generated keys become recognized objects; unknown names are reported
// and preserved.
func (b *Backend) Inventory(
	ctx context.Context,
	cursor packstore.InventoryCursor,
) (packstore.InventoryPage, error) {
	output, err := s3io.List(ctx, b.client, b.bucket, b.keys.join(""), string(cursor), b.page)
	if err != nil {
		return packstore.InventoryPage{}, classifyError("list store inventory", err)
	}
	page := packstore.InventoryPage{}
	for _, object := range output.Contents {
		key := aws.ToString(object.Key)
		if key == b.keys.ownership() {
			continue
		}
		if ref, ok := b.keys.objectRef(key); ok {
			page.Objects = append(page.Objects, packstore.InventoryObject{
				Ref: ref, StoredSize: aws.ToInt64(object.Size),
			})
			continue
		}
		page.Unknown = append(page.Unknown, key)
	}
	sort.Slice(page.Objects, func(i, j int) bool {
		return inventoryRefKey(page.Objects[i].Ref) < inventoryRefKey(page.Objects[j].Ref)
	})
	sort.Strings(page.Unknown)
	if output.IsTruncated != nil && *output.IsTruncated {
		page.NextCursor = packstore.InventoryCursor(aws.ToString(output.NextContinuationToken))
	}
	return page, nil
}

// Retire deletes one canonical object after a fresh ownership check.
func (b *Backend) Retire(ctx context.Context, ref packstore.ObjectRef) error {
	if _, err := b.requireOwnership(ctx); err != nil {
		return err
	}
	key, err := b.objectKey(ref)
	if err != nil {
		return err
	}
	_, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	return classifyError("retire object", err)
}

func (b *Backend) objectKey(ref packstore.ObjectRef) (string, error) {
	switch {
	case ref.LooseHash != "" && ref.PackID == "":
		if err := ref.LooseHash.Validate(); err != nil {
			return "", err
		}
		if ref.LooseEncoding != packstore.LooseEncodingRaw &&
			ref.LooseEncoding != packstore.LooseEncodingZstd {
			return "", fmt.Errorf("s3store: invalid loose encoding %d", ref.LooseEncoding)
		}
		return b.keys.loose(ref.LooseHash, ref.LooseEncoding), nil
	case ref.LooseHash == "" && ref.LooseEncoding == 0 && ref.PackID != "":
		if !pack.IsValidPackID(ref.PackID) {
			return "", fmt.Errorf("s3store: invalid pack id %q", ref.PackID)
		}
		return b.keys.pack(ref.PackID), nil
	default:
		return "", errors.New("s3store: object reference must select one representation")
	}
}

func inventoryRefKey(ref packstore.ObjectRef) string {
	if ref.LooseHash != "" {
		return fmt.Sprintf("loose/%d/%s", ref.LooseEncoding, ref.LooseHash)
	}
	return "pack/" + ref.PackID
}

// Probe verifies the endpoint capabilities required for safe operation using
// an owned, temporary probe key. Endpoints that cannot provide strong,
// repeatable reads are rejected rather than accommodated with retry windows.
func (b *Backend) Probe(ctx context.Context) (report CapabilityReport, resultErr error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return report, err
	}
	generation, err := newGeneration()
	if err != nil {
		return report, err
	}
	key := b.keys.staging(owner.Epoch, string(generation), "probe")
	payload := bytes.Repeat([]byte("kit-s3-probe\n"), 512<<10)
	published, err := b.multipartPublish(ctx, key, bytes.NewReader(payload), multipartPublishOptions{
		maxBytes: int64(len(payload)), exactSize: int64(len(payload)), sizeKnown: true,
	})
	if err != nil {
		return report, err
	}
	if !published.created {
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: fresh probe key already exists"),
		)
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			resultErr = errors.Join(resultErr, b.cleanupProbeObject(ctx, key))
		}
	}()

	for index := range 2 {
		output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key),
		})
		if err != nil {
			return report, classifyError("probe read-after-write", err)
		}
		got, readErr := readProbeBody(output.Body, output.ContentLength, payload)
		closeErr := output.Body.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
			return report, errors.Join(
				packstore.ErrPhysicalCorrupt, readErr, closeErr,
				fmt.Errorf("s3store: probe read %d differs from publication", index+1),
			)
		}
	}
	report.StrongReadAfterWrite = true
	report.RepeatableReads = true
	report.MultipartPublication = true

	ranged, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Range: aws.String("bytes=5-20"),
	})
	if err != nil {
		return report, classifyError("probe range read", err)
	}
	got, readErr := readProbeBody(ranged.Body, ranged.ContentLength, payload[5:21])
	closeErr := ranged.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, payload[5:21]) {
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt, readErr, closeErr,
			errors.New("s3store: probe range differs from publication"),
		)
	}
	report.RangeReads = true

	listed, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(2),
	})
	if err != nil {
		return report, classifyError("probe listing", err)
	}
	found := false
	for _, object := range listed.Contents {
		found = found || aws.ToString(object.Key) == key
	}
	if !found {
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: probe listing omitted published key"),
		)
	}
	report.Listing = true

	replacement := []byte("kit-s3-conditional-replacement\n")
	duplicate, err := b.multipartPublish(
		ctx,
		key,
		bytes.NewReader(replacement),
		multipartPublishOptions{
			maxBytes: int64(len(replacement)), exactSize: int64(len(replacement)), sizeKnown: true,
		},
	)
	if err != nil {
		return report, err
	}
	if duplicate.created {
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: endpoint ignored conditional multipart creation"),
		)
	}
	probeETag, err := b.verifyProbeObject(ctx, key, payload, "conditional multipart creation")
	if err != nil {
		return report, err
	}
	_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body: bytes.NewReader(replacement), ContentLength: aws.Int64(int64(len(replacement))),
		IfMatch: aws.String(probeETag),
	})
	if err != nil {
		return report, classifyError("probe conditional replacement", err)
	}
	staleReplacement := []byte("kit-s3-stale-conditional-replacement\n")
	_, staleErr := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body:          bytes.NewReader(staleReplacement),
		ContentLength: aws.Int64(int64(len(staleReplacement))),
		IfMatch:       aws.String(probeETag),
	})
	if statusCode(staleErr) != http.StatusPreconditionFailed {
		if staleErr != nil {
			return report, classifyError("probe stale conditional replacement", staleErr)
		}
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: endpoint ignored stale conditional replacement"),
		)
	}
	if _, err := b.verifyProbeObject(ctx, key, replacement, "stale conditional replacement"); err != nil {
		return report, err
	}
	report.ConditionalWrites = true

	if _, err := b.requireOwnership(ctx); err != nil {
		return report, err
	}
	_, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return report, classifyError("probe delete", err)
	}
	_, err = b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err == nil {
		return report, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: endpoint acknowledged probe deletion but object remains"),
		)
	}
	if statusCode(err) != http.StatusNotFound {
		return report, classifyError("verify probe delete", err)
	}
	cleanupPending = false
	resultErr = nil
	report.Delete = true
	return report, nil
}

func (b *Backend) verifyProbeObject(
	ctx context.Context,
	key string,
	expected []byte,
	operation string,
) (string, error) {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return "", classifyError("verify probe "+operation, err)
	}
	got, readErr := readProbeBody(output.Body, output.ContentLength, expected)
	closeErr := output.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, expected) {
		return "", errors.Join(
			packstore.ErrPhysicalCorrupt,
			readErr,
			closeErr,
			fmt.Errorf("s3store: probe object changed after %s", operation),
		)
	}
	etag := aws.ToString(output.ETag)
	if etag == "" {
		return "", errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: probe object has no ETag after %s", operation),
		)
	}
	return etag, nil
}

func readProbeBody(body io.Reader, contentLength *int64, expected []byte) ([]byte, error) {
	if contentLength != nil && *contentLength != int64(len(expected)) {
		return nil, fmt.Errorf(
			"s3store: probe response length is %d, want %d",
			*contentLength, len(expected),
		)
	}
	limited := io.LimitReader(body, int64(len(expected))+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > len(expected) {
		return nil, fmt.Errorf(
			"s3store: probe response exceeds expected length %d", len(expected),
		)
	}
	return data, nil
}

func (b *Backend) cleanupProbeObject(ctx context.Context, key string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeCleanupTimeout)
	defer cancel()
	if _, err := b.requireOwnership(cleanupCtx); err != nil {
		return err
	}
	_, err := b.client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	return classifyError("clean probe object", err)
}

// NamespaceEmpty reports whether the configured bucket prefix contains any
// object before ownership is attached.
func (b *Backend) NamespaceEmpty(ctx context.Context) (bool, error) {
	output, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket), Prefix: aws.String(b.keys.join("")),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, classifyError("inspect namespace", err)
	}
	return len(output.Contents) == 0, nil
}
