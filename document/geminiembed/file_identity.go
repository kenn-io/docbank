package geminiembed

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const fileClockSkew = 5 * time.Minute

func validateUploadURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != host ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" ||
		parsed.Path != filesUploadPath || !validUploadQuery(parsed.RawQuery) {
		return nil, errors.New("gemini embed: provider upload URL is outside sealed egress")
	}
	return parsed, nil
}

func validUploadQuery(rawQuery string) bool {
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query) != 2 || len(query["upload_id"]) != 1 ||
		len(query["upload_protocol"]) != 1 || query.Get("upload_protocol") != "resumable" {
		return false
	}
	uploadID := query.Get("upload_id")
	if len(uploadID) == 0 || len(uploadID) > 1024 {
		return false
	}
	for _, character := range uploadID {
		if character != '-' && character != '_' &&
			(character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	first := "upload_id=" + uploadID + "&upload_protocol=resumable"
	second := "upload_protocol=resumable&upload_id=" + uploadID
	return rawQuery == first || rawQuery == second
}

type validatedProviderFile struct {
	file      wireFile
	created   time.Time
	updated   time.Time
	expiresAt time.Time
}

func validateWireFile(value wireFile, expected verifiedFile) (validatedProviderFile, bool) {
	if !validFileName(value.Name) || value.URI != origin+"/v1beta/"+value.Name ||
		value.MIMEType != expected.metadata.MediaType || value.Source != "UPLOADED" || value.Error != nil ||
		value.State != "PROCESSING" && value.State != "ACTIVE" && value.State != "FAILED" ||
		len(value.DisplayName) > 512 {
		return validatedProviderFile{}, false
	}
	size, err := strconv.ParseInt(value.SizeBytes, 10, 64)
	if err != nil || size != expected.metadata.ByteLength {
		return validatedProviderFile{}, false
	}
	hash, err := base64.StdEncoding.Strict().DecodeString(value.SHA256Hash)
	if err != nil || !matchesFileHash(hash, expected.metadata.SHA256) {
		return validatedProviderFile{}, false
	}
	created, createErr := time.Parse(time.RFC3339Nano, value.CreateTime)
	updated, updateErr := time.Parse(time.RFC3339Nano, value.UpdateTime)
	expires, expiryErr := time.Parse(time.RFC3339Nano, value.ExpirationTime)
	if createErr != nil || updateErr != nil || expiryErr != nil || updated.Before(created) ||
		updated.After(expires) || !expires.After(created) || expires.Sub(created) > retentionCeiling {
		return validatedProviderFile{}, false
	}
	return validatedProviderFile{file: value, created: created, updated: updated, expiresAt: expires}, true
}

func validateCreatedWireFile(value wireFile, expected verifiedFile, startedAt, completedAt time.Time) (validatedProviderFile, bool) {
	validated, ok := validateWireFile(value, expected)
	if !ok || validated.created.Before(startedAt.Add(-fileClockSkew)) ||
		validated.created.After(completedAt.Add(fileClockSkew)) ||
		validated.updated.Before(startedAt.Add(-fileClockSkew)) ||
		validated.updated.After(completedAt.Add(fileClockSkew)) ||
		!validated.expiresAt.After(completedAt) {
		return validatedProviderFile{}, false
	}
	return validated, true
}

func validatePolledWireFile(value wireFile, expected verifiedFile, current validatedProviderFile, startedAt, completedAt time.Time) (validatedProviderFile, bool) {
	validated, ok := validateWireFile(value, expected)
	if !ok || value.Name != current.file.Name || value.URI != current.file.URI ||
		!validated.created.Equal(current.created) || !validated.expiresAt.Equal(current.expiresAt) ||
		validated.updated.Before(current.updated) || validated.updated.Before(startedAt.Add(-fileClockSkew)) ||
		validated.updated.After(completedAt.Add(fileClockSkew)) || !validated.expiresAt.After(completedAt) {
		return validatedProviderFile{}, false
	}
	return validated, true
}

func matchesFileHash(decoded []byte, expectedHex string) bool {
	return len(decoded) == sha256.Size && hex.EncodeToString(decoded) == expectedHex ||
		len(decoded) == sha256.Size*2 && string(decoded) == expectedHex
}

func validFileName(name string) bool {
	id := strings.TrimPrefix(name, "files/")
	if id == name || len(id) == 0 || len(id) > 40 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for _, character := range id {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
