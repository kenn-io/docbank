package document

import "time"

// ProcessingConsentRequest explicitly authorizes one profile and disclosure
// scope in the current vault incarnation. It never follows an email relation.
type ProcessingConsentRequest struct {
	Principal               string     `json:"principal"`
	Scope                   string     `json:"scope"`
	ProfileFingerprint      string     `json:"profile_fingerprint"`
	DisclosureFingerprint   string     `json:"disclosure_fingerprint"`
	InputClasses            []string   `json:"input_classes"`
	RetainedArtifactClasses []string   `json:"retained_artifact_classes"`
	ExpiresAt               *time.Time `json:"expires_at"`
}
type ProcessingConsentReceipt struct {
	GrantID                 string    `json:"grant_id"`
	VaultID                 string    `json:"vault_id"`
	ProcessingIncarnationID string    `json:"processing_incarnation_id"`
	RevocationFence         int64     `json:"revocation_fence"`
	IssuedAt                time.Time `json:"issued_at"`
}
type ProcessingConsentRevocationRequest struct {
	Principal string `json:"principal"`
	Scope     string `json:"scope"`
}
type ProcessingConsentRevocationReceipt struct {
	RevocationID            string    `json:"revocation_id"`
	ProcessingIncarnationID string    `json:"processing_incarnation_id"`
	Fence                   int64     `json:"fence"`
	RevokedAt               time.Time `json:"revoked_at"`
}
