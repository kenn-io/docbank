package processing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
)

const mediaPlanTokenDomain = "media-acquisition-plan/v1\x00" //nolint:gosec // HMAC domain separator, not a credential.

var (
	ErrMediaCapabilityUnavailable = errors.New("media acquisition capability is unavailable")
	ErrMediaPlanInvalid           = errors.New("media acquisition plan is invalid")
	ErrMediaPlanExpired           = errors.New("media acquisition plan has expired")
)

// MediaOriginPolicy is a local, versioned recognition and disclosure policy.
// It contains no credential values or private reference material.
type MediaOriginPolicy struct {
	OriginID, Provider, ResolverFingerprint, IdentityFingerprint, DisclosureFingerprint string
	InputClasses, RetainedClasses                                                       []string
	ReferencePrefixes                                                                   []string
	ExactOrigin                                                                         string                                  `json:",omitzero"`
	CredentialBinding                                                                   string                                  `json:",omitzero"`
	RecognizePath                                                                       func(escapedPath string) (string, bool) `json:"-"`
	AcquisitionAvailable                                                                bool
}

type MediaOrigin struct {
	OriginID, Provider, AdapterContract, DeploymentRevision, ProbeState string
	AcquisitionAvailable                                                bool
	ProbedAt                                                            time.Time
}

type MediaOriginEvidence struct {
	AdapterContract, DeploymentRevision, ProbeState string
}

type MediaOriginProbe func(context.Context) (MediaOriginEvidence, error)

type MediaAcquisitionPlan struct {
	PlanToken, PlanFingerprint, OriginID, Provider, GrantState string
	InputClasses, RetainedClasses                              []string
}

type mediaAcquisitionClaim struct {
	Version, Principal, IncarnationID, OriginID, Provider string
	ReferenceSHA256, ConfigurationFingerprint             string
	ProfileFingerprint, DisclosureFingerprint             string
	InputClasses, RetainedClasses                         []string
	IssuedAt, ExpiresAt                                   string
}

func (service *Service) MediaOrigins() ([]MediaOrigin, error) {
	if service == nil || len(service.mediaOrigins) == 0 {
		return nil, ErrMediaCapabilityUnavailable
	}
	service.mediaOriginMu.Lock()
	defer service.mediaOriginMu.Unlock()
	result := make([]MediaOrigin, 0, len(service.mediaOrigins))
	for id, policy := range service.mediaOrigins {
		origin := MediaOrigin{OriginID: id, Provider: policy.Provider,
			AcquisitionAvailable: policy.AcquisitionAvailable}
		if probe, ok := service.mediaOriginProbes[id]; ok && probe != nil {
			if observation, observed := service.mediaOriginEvidence[id]; observed {
				origin.AdapterContract = observation.Evidence.AdapterContract
				origin.DeploymentRevision = observation.Evidence.DeploymentRevision
				origin.ProbeState = observation.Evidence.ProbeState
				origin.ProbedAt = observation.ProbedAt
			} else {
				origin.ProbeState = "pending"
			}
		}
		result = append(result, origin)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OriginID < result[j].OriginID })
	return result, nil
}

// ProbeMediaOrigins runs each configured probe and retains its bounded result.
func (service *Service) ProbeMediaOrigins(ctx context.Context) error {
	if service == nil {
		return ErrMediaCapabilityUnavailable
	}
	ids := make([]string, 0, len(service.mediaOriginProbes))
	for id := range service.mediaOriginProbes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		evidence, err := service.mediaOriginProbes[id](ctx)
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if err != nil && evidence.ProbeState == "" {
			evidence.ProbeState = "provider_unavailable"
		}
		service.mediaOriginMu.Lock()
		service.mediaOriginEvidence[id] = mediaOriginObservation{Evidence: evidence, ProbedAt: service.clock().UTC()}
		service.mediaOriginMu.Unlock()
	}
	return nil
}

// PlanMediaAcquisition recognizes a private reference locally. The signed
// token contains only its digest and sanitized current policy identities.
func (service *Service) PlanMediaAcquisition(
	ctx context.Context, request RemoteRecordingRequest,
) (MediaAcquisitionPlan, error) {
	if service == nil || len(service.mediaOrigins) == 0 {
		return MediaAcquisitionPlan{}, ErrMediaCapabilityUnavailable
	}
	if request.ReferenceURL == "" || len(request.ReferenceURL) > 8192 || len(request.CanonicalURL) > 8192 {
		return MediaAcquisitionPlan{}, ErrMediaPlanInvalid
	}
	policy, _, ok := service.recognizeMediaOrigin(request.ReferenceURL, request.ProviderHint)
	if !ok || !policy.AcquisitionAvailable {
		return MediaAcquisitionPlan{}, ErrMediaCapabilityUnavailable
	}
	configuration, err := mediaOriginFingerprint(policy)
	if err != nil {
		return MediaAcquisitionPlan{}, err
	}
	incarnation, err := service.catalog.CurrentProcessingIncarnation(ctx)
	if err != nil {
		return MediaAcquisitionPlan{}, err
	}
	now := service.clock().UTC()
	referenceDigest := sha256.Sum256([]byte(request.ReferenceURL))
	referenceSHA256 := hex.EncodeToString(referenceDigest[:])
	consentProfileFingerprint := stableHash(
		"docbank/media-acquisition-consent/v1", configuration, referenceSHA256)
	claim := mediaAcquisitionClaim{Version: "v1", Principal: service.principal,
		IncarnationID: incarnation.ID, OriginID: policy.OriginID, Provider: policy.Provider,
		ReferenceSHA256: referenceSHA256, ConfigurationFingerprint: configuration,
		ProfileFingerprint: consentProfileFingerprint, DisclosureFingerprint: policy.DisclosureFingerprint,
		InputClasses: slices.Clone(policy.InputClasses), RetainedClasses: slices.Clone(policy.RetainedClasses),
		IssuedAt: now.Format(timestampForm), ExpiresAt: now.Add(15 * time.Minute).Format(timestampForm)}
	body, err := canonical.Marshal(claim)
	if err != nil {
		return MediaAcquisitionPlan{}, err
	}
	token, err := signMediaToken(service.mediaTokenKey, body)
	if err != nil {
		return MediaAcquisitionPlan{}, err
	}
	planDigest := sha256.Sum256(body)
	grantState := "required"
	_, authErr := service.catalog.AuthorizeProviderOperation(ctx, mediaAcquisitionAuthorization(service, claim, nil))
	if authErr == nil {
		grantState = "granted"
	} else if !errors.Is(authErr, store.ErrProcessingConsentRequired) &&
		!errors.Is(authErr, store.ErrProcessingConsentExpired) &&
		!errors.Is(authErr, store.ErrProcessingConsentRevoked) {
		return MediaAcquisitionPlan{}, authErr
	}
	return MediaAcquisitionPlan{PlanToken: token, PlanFingerprint: hex.EncodeToString(planDigest[:]),
		OriginID: policy.OriginID, Provider: policy.Provider, InputClasses: slices.Clone(policy.InputClasses),
		RetainedClasses: slices.Clone(policy.RetainedClasses), GrantState: grantState}, nil
}

func (service *Service) GrantMediaAcquisition(
	ctx context.Context, operationID, token string, expiresAt *time.Time,
) (store.MediaConsentReceipt, error) {
	if service == nil {
		return store.MediaConsentReceipt{}, ErrMediaCapabilityUnavailable
	}
	expiresRaw := ""
	if expiresAt != nil {
		expiresRaw = expiresAt.UTC().Format(timestampForm)
	}
	requestIdentity, err := canonical.Marshal(struct {
		PlanToken, ExpiresAt string
	}{token, expiresRaw})
	if err != nil {
		return store.MediaConsentReceipt{}, err
	}
	requestDigest := sha256.Sum256(requestIdentity)
	op := store.MediaOperation{ID: operationID, Principal: service.principal,
		Verb: "grant_media_acquisition", RequestSHA256: hex.EncodeToString(requestDigest[:])}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, op); replayErr == nil {
		return canonical.Decode[store.MediaConsentReceipt]([]byte(replay))
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return store.MediaConsentReceipt{}, replayErr
	}
	if expiresAt != nil && !service.clock().UTC().Before(expiresAt.UTC()) {
		return store.MediaConsentReceipt{}, ErrInvalidConsentExpiry
	}
	claim, policy, err := service.verifyCurrentMediaPlan(ctx, token)
	if err != nil {
		return store.MediaConsentReceipt{}, err
	}
	return service.catalog.GrantMediaAcquisitionConsent(ctx, op, policy.OriginID, store.ProcessingConsentGrantRequest{
		Principal: service.principal, Scope: mediaAcquisitionScope(policy.OriginID),
		ProfileFingerprint: claim.ProfileFingerprint, DisclosureFingerprint: claim.DisclosureFingerprint,
		InputClasses: claim.InputClasses, RetainedArtifactClasses: claim.RetainedClasses, ExpiresAt: expiresAt,
	})
}

func (service *Service) RevokeMediaAcquisition(
	ctx context.Context, operationID, originID string,
) (store.MediaConsentReceipt, error) {
	if service == nil {
		return store.MediaConsentReceipt{}, ErrMediaCapabilityUnavailable
	}
	digest := sha256.Sum256([]byte(originID))
	op := store.MediaOperation{ID: operationID, Principal: service.principal,
		Verb: "revoke_media_acquisition", RequestSHA256: hex.EncodeToString(digest[:])}
	if replay, replayErr := service.catalog.MediaOperationReceipt(ctx, op); replayErr == nil {
		return canonical.Decode[store.MediaConsentReceipt]([]byte(replay))
	} else if !errors.Is(replayErr, store.ErrNotFound) {
		return store.MediaConsentReceipt{}, replayErr
	}
	if len(service.mediaOrigins) == 0 {
		return store.MediaConsentReceipt{}, ErrMediaCapabilityUnavailable
	}
	policy, ok := service.mediaOrigins[originID]
	if !ok || policy.OriginID != originID {
		return store.MediaConsentReceipt{}, ErrMediaCapabilityUnavailable
	}
	return service.catalog.RevokeMediaAcquisitionConsent(ctx, op, originID, store.ProcessingConsentRevocationRequest{
		Principal: service.principal, Scope: mediaAcquisitionScope(originID),
	})
}

func (service *Service) verifyCurrentMediaPlan(
	ctx context.Context, token string,
) (mediaAcquisitionClaim, MediaOriginPolicy, error) {
	if service == nil || len(service.mediaOrigins) == 0 {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaCapabilityUnavailable
	}
	body, err := verifyMediaToken(service.mediaTokenKey, token)
	if err != nil {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaPlanInvalid
	}
	var claim mediaAcquisitionClaim
	if err := json.Unmarshal(body, &claim, json.RejectUnknownMembers(true)); err != nil || claim.Version != "v1" {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaPlanInvalid
	}
	issuedAt, issueErr := time.Parse(timestampForm, claim.IssuedAt)
	expiresAt, expiryErr := time.Parse(timestampForm, claim.ExpiresAt)
	now := service.clock().UTC()
	if issueErr != nil || expiryErr != nil || expiresAt.Sub(issuedAt) != 15*time.Minute || now.Before(issuedAt) {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaPlanInvalid
	}
	if !now.Before(expiresAt) {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaPlanExpired
	}
	incarnation, err := service.catalog.CurrentProcessingIncarnation(ctx)
	if err != nil {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, err
	}
	policy, ok := service.mediaOrigins[claim.OriginID]
	configuration, fingerprintErr := mediaOriginFingerprint(policy)
	wantConsentProfile := stableHash(
		"docbank/media-acquisition-consent/v1", configuration, claim.ReferenceSHA256)
	if fingerprintErr != nil || !ok || claim.Principal != service.principal || claim.IncarnationID != incarnation.ID ||
		claim.Provider != policy.Provider || claim.ConfigurationFingerprint != configuration ||
		claim.ProfileFingerprint != wantConsentProfile || claim.DisclosureFingerprint != policy.DisclosureFingerprint ||
		!slices.Equal(claim.InputClasses, policy.InputClasses) || !slices.Equal(claim.RetainedClasses, policy.RetainedClasses) {
		return mediaAcquisitionClaim{}, MediaOriginPolicy{}, ErrMediaPlanInvalid
	}
	return claim, policy, nil
}

func (service *Service) recognizeMediaOrigin(reference, providerHint string) (MediaOriginPolicy, string, bool) {
	canonicalReference, origin, canonicalErr := canonicalRemoteRecordingReference(reference)
	if canonicalErr == nil {
		parsed, err := url.Parse(canonicalReference)
		if err == nil {
			escapedPath := parsed.EscapedPath()
			for _, policy := range service.mediaOrigins {
				if origin != policy.ExactOrigin || policy.RecognizePath == nil {
					continue
				}
				if identity, ok := policy.RecognizePath(escapedPath); ok {
					return policy, identity, true
				}
			}
		}
	}
	for _, policy := range service.mediaOrigins {
		if providerHint != "" && providerHint != policy.Provider {
			continue
		}
		for _, prefix := range policy.ReferencePrefixes {
			if strings.HasPrefix(reference, prefix) {
				return policy, "", true
			}
		}
	}
	return MediaOriginPolicy{}, "", false
}

func mediaOriginFingerprint(policy MediaOriginPolicy) (string, error) {
	encoded, err := canonical.Marshal(policy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func mediaAcquisitionAuthorization(service *Service, claim mediaAcquisitionClaim, expiresAt *time.Time) store.ProviderOperationAuthorizationRequest {
	_ = expiresAt
	return store.ProviderOperationAuthorizationRequest{Principal: service.principal,
		Scope: mediaAcquisitionScope(claim.OriginID), ProfileFingerprint: claim.ProfileFingerprint,
		DisclosureFingerprint: claim.DisclosureFingerprint, InputClasses: slices.Clone(claim.InputClasses),
		RetainedArtifactClasses: slices.Clone(claim.RetainedClasses)}
}

func mediaAcquisitionScope(originID string) string { return "media:acquire:" + originID }

func signMediaToken(key [32]byte, body []byte) (string, error) {
	if len(body) > 12000 {
		return "", errors.New("media plan exceeds bounds")
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(mediaPlanTokenDomain))
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyMediaToken(key [32]byte, token string) ([]byte, error) {
	fail := errors.New("invalid media acquisition plan")
	if len(token) > 16384 {
		return nil, fail
	}
	encodedBody, encodedSignature, ok := strings.Cut(token, ".")
	if !ok {
		return nil, fail
	}
	body, err := base64.RawURLEncoding.DecodeString(encodedBody)
	if err != nil || len(body) > 12000 {
		return nil, fail
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return nil, fail
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(mediaPlanTokenDomain))
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, fail
	}
	return body, nil
}
