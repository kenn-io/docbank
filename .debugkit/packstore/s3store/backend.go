package s3store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.kenn.io/kit/packstore"
)

const (
	defaultPartBytes     = int64(8 << 20)
	defaultInventorySize = int32(1000)
	maximumPartBytes     = int64(5 << 30)
)

// Config binds one S3-compatible namespace. The bucket must already exist.
// ExpectedOwnership enables ordinary reads and destructive operations; an
// unattached backend may only inspect or replace its ownership marker.
type Config struct {
	Endpoint       string
	Region         string
	Bucket         string
	Prefix         string
	Credentials    aws.CredentialsProvider
	ForcePathStyle bool
	// AllowInsecureTransport permits an explicitly configured HTTP endpoint.
	// It does not relax URL parsing or permit non-HTTP schemes.
	AllowInsecureTransport bool
	ExpectedOwnership      *packstore.Ownership
	PartBytes              int64
	InventoryPageSize      int32
	Limits                 packstore.Limits
}

// CapabilityReport records the endpoint behavior required by this backend.
type CapabilityReport struct {
	StrongReadAfterWrite bool
	RepeatableReads      bool
	RangeReads           bool
	MultipartPublication bool
	ConditionalWrites    bool
	Listing              bool
	Delete               bool
}

// Backend owns S3-compatible byte mechanics without granting catalog
// authority.
type Backend struct {
	client *s3.Client
	bucket string
	keys   keyspace
	part   int64
	page   int32
	limits packstore.Limits
	mu     sync.RWMutex
	owner  *packstore.Ownership
	etag   string
}

// New constructs a backend and validates its static configuration. Probe
// performs the mutating endpoint-capability checks separately.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if ctx == nil {
		return nil, errors.New("s3store: nil context")
	}
	if err := validateEndpoint(cfg.Endpoint, cfg.AllowInsecureTransport); err != nil {
		return nil, err
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3store: bucket is required")
	}
	keys, err := newKeyspace(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PartBytes == 0 {
		cfg.PartBytes = defaultPartBytes
	}
	if err := validatePartBytes(cfg.PartBytes, uint64(^uint(0)>>1)); err != nil {
		return nil, err
	}
	if cfg.InventoryPageSize == 0 {
		cfg.InventoryPageSize = defaultInventorySize
	}
	if cfg.InventoryPageSize < 1 || cfg.InventoryPageSize > 1000 {
		return nil, errors.New("s3store: inventory page size must be between 1 and 1000")
	}
	cfg.Limits, err = normalizeLimits(cfg.Limits)
	if err != nil {
		return nil, err
	}
	if cfg.ExpectedOwnership != nil {
		if err := cfg.ExpectedOwnership.Validate(); err != nil {
			return nil, err
		}
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.Credentials != nil {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(cfg.Credentials))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("s3store: load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.ForcePathStyle
		// Config.Endpoint is the only admitted custom endpoint; SDK
		// environment and shared-config overrides bypass its validation.
		options.BaseEndpoint = nil
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	backend := &Backend{
		client: client, bucket: cfg.Bucket, keys: keys,
		part: cfg.PartBytes, page: cfg.InventoryPageSize, limits: cfg.Limits,
	}
	if cfg.ExpectedOwnership != nil {
		cloned := *cfg.ExpectedOwnership
		backend.owner = &cloned
	}
	return backend, nil
}

func validatePartBytes(partBytes int64, platformMaxInt uint64) error {
	if partBytes < 5<<20 {
		return errors.New("s3store: multipart part size must be at least 5 MiB")
	}
	if partBytes > maximumPartBytes {
		return errors.New("s3store: multipart part size must be at most 5 GiB")
	}
	if uint64(partBytes) > platformMaxInt {
		return errors.New("s3store: multipart part size exceeds platform int maximum")
	}
	return nil
}

func validateEndpoint(endpoint string, allowInsecure bool) error {
	if endpoint == "" {
		return nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return errors.New("s3store: endpoint must be an absolute HTTP or HTTPS URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return errors.New("s3store: endpoint must be an absolute HTTP or HTTPS URL")
	}
	if scheme == "http" && !allowInsecure {
		return errors.New("s3store: insecure HTTP endpoint requires AllowInsecureTransport")
	}
	return nil
}

func normalizeLimits(limits packstore.Limits) (packstore.Limits, error) {
	if limits == (packstore.Limits{}) {
		limits = packstore.DefaultLimits()
	}
	if limits.BlobBytes <= 0 || limits.PackBytes <= 0 ||
		limits.FooterBytes <= 0 || limits.PackEntries <= 0 {
		return packstore.Limits{}, errors.New("s3store: invalid pack reader limits")
	}
	return limits, nil
}

// StoreID returns the configured stable store identity, or empty while
// unattached.
func (b *Backend) StoreID() packstore.StoreID {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.owner == nil {
		return ""
	}
	return b.owner.Store
}

func (b *Backend) expectedOwnership() (*packstore.Ownership, string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.owner == nil {
		return nil, ""
	}
	cloned := *b.owner
	return &cloned, b.etag
}

func (b *Backend) setOwnership(owner packstore.Ownership, etag string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cloned := owner
	b.owner = &cloned
	b.etag = etag
}
