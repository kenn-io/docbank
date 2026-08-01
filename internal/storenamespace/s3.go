// Package storenamespace canonicalizes deployment storage namespaces before
// Docbank compares or grants authority to them.
package storenamespace

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Binding contains the non-secret fields that identify an S3 namespace.
type S3Binding struct {
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
}

// S3 identifies one canonical S3-compatible object namespace.
type S3 struct {
	Endpoint string
	Bucket   string
	Prefix   string
}

// CanonicalS3 validates and canonicalizes one configured S3 namespace.
func CanonicalS3(binding S3Binding) (S3, error) {
	endpoint, err := CanonicalS3Endpoint(binding.Endpoint, binding.Region)
	if err != nil {
		return S3{}, err
	}
	if binding.Bucket == "" {
		return S3{}, errors.New("bucket is required")
	}
	if err := validateS3Prefix(binding.Prefix); err != nil {
		return S3{}, err
	}
	return S3{
		Endpoint: endpoint,
		Bucket:   strings.ToLower(binding.Bucket),
		Prefix:   binding.Prefix,
	}, nil
}

// S3Overlaps reports whether two bindings can address any of the same keys.
func S3Overlaps(first, second S3Binding) (bool, error) {
	left, err := CanonicalS3(first)
	if err != nil {
		return false, err
	}
	right, err := CanonicalS3(second)
	if err != nil {
		return false, err
	}
	if left.Endpoint != right.Endpoint || left.Bucket != right.Bucket {
		return false, nil
	}
	return prefixContains(left.Prefix, right.Prefix) ||
		prefixContains(right.Prefix, left.Prefix), nil
}

// CanonicalS3Endpoint normalizes explicit endpoints and maps AWS endpoints to
// their SDK partition identity so implicit and explicit forms compare equal.
func CanonicalS3Endpoint(raw, region string) (string, error) {
	if region == "" {
		region = "us-east-1"
	}
	if raw == "" {
		return awsS3Partition(region)
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", raw, err)
	}
	if endpoint.Scheme == "" || endpoint.Host == "" {
		return "", fmt.Errorf("endpoint %q must include scheme and host", raw)
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", fmt.Errorf("endpoint %q must not include user info, query, or fragment", raw)
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	if parsed := net.ParseIP(host); parsed != nil {
		host = parsed.String()
	}
	port := endpoint.Port()
	if (endpoint.Scheme == "https" && port == "443") ||
		(endpoint.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(host, ":") {
			endpoint.Host = "[" + host + "]"
		} else {
			endpoint.Host = host
		}
	} else {
		endpoint.Host = net.JoinHostPort(host, port)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/")
	if partition, ok := canonicalAWSS3Partition(endpoint, region); ok {
		return partition, nil
	}
	return endpoint.String(), nil
}

func validateS3Prefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, `\`) ||
		strings.Contains(prefix, "//") || path.Clean(prefix) != prefix ||
		prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") {
		return fmt.Errorf("prefix %q is not canonical", prefix)
	}
	return nil
}

func prefixContains(parent, child string) bool {
	return parent == "" || parent == child || strings.HasPrefix(child, parent+"/")
}

func canonicalAWSS3Partition(endpoint *url.URL, region string) (string, bool) {
	if endpoint.Port() != "" || endpoint.Path != "" || region == "" {
		return "", false
	}
	resolver := awss3.NewDefaultEndpointResolver()
	variants := []awss3.EndpointResolverOptions{
		{},
		{UseFIPSEndpoint: aws.FIPSEndpointStateEnabled},
		{UseDualStackEndpoint: aws.DualStackEndpointStateEnabled},
		{
			UseFIPSEndpoint:      aws.FIPSEndpointStateEnabled,
			UseDualStackEndpoint: aws.DualStackEndpointStateEnabled,
		},
	}
	for _, options := range variants {
		resolved, err := resolver.ResolveEndpoint(region, options)
		if err != nil {
			continue
		}
		resolvedURL, err := url.Parse(resolved.URL)
		if err == nil && strings.EqualFold(endpoint.Hostname(), resolvedURL.Hostname()) {
			return resolved.PartitionID, true
		}
	}
	return "", false
}

func awsS3Partition(region string) (string, error) {
	resolved, err := awss3.NewDefaultEndpointResolver().ResolveEndpoint(
		region, awss3.EndpointResolverOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("resolve AWS S3 region %q: %w", region, err)
	}
	return resolved.PartitionID, nil
}
