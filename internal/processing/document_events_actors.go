package processing

import (
	"fmt"
	"net/mail"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type metadataActorClaim struct {
	fieldKey string
	actor    document.DocumentEventActorV1
}

func metadataActorsForField(field document.SourceMetadataFieldV1) ([]metadataActorClaim, error) {
	actors := make([]metadataActorClaim, 0)
	if field.Key == "creators" && field.Value.Kind == document.SourceMetadataStringList {
		for index, value := range field.Value.Strings {
			if len(value) > document.MaxDocumentEventActorClaimBytes {
				return nil, fmt.Errorf("metadata actor claim exceeds %d bytes: %w",
					document.MaxDocumentEventActorClaimBytes, store.ErrDocumentEventEvidenceUnavailable)
			}
			key, err := document.ActorKeyV1("name_alias", value)
			if err != nil {
				continue
			}
			actors = append(actors, metadataActorClaim{fieldKey: field.Key, actor: document.DocumentEventActorV1{
				ActorKey: key, Claim: value, DisplayName: value, Ordinal: index,
				Role: "author", Sensitive: field.Sensitive,
			}})
		}
		return actors, nil
	}
	role, ok := metadataEmailActorRole(field.Key)
	if !ok || field.Value.Kind != document.SourceMetadataString || field.Value.String == nil {
		return actors, nil
	}
	if len(*field.Value.String) > document.MaxDocumentEventActorClaimBytes {
		return nil, fmt.Errorf("metadata actor claim exceeds %d bytes: %w",
			document.MaxDocumentEventActorClaimBytes, store.ErrDocumentEventEvidenceUnavailable)
	}
	addresses, err := mail.ParseAddressList(*field.Value.String)
	if err == nil {
		for index, address := range addresses {
			key, keyErr := document.ActorKeyV1("email", address.Address)
			if keyErr != nil {
				continue
			}
			actors = append(actors, metadataActorClaim{fieldKey: field.Key, actor: document.DocumentEventActorV1{
				ActorKey: key, Address: address.Address, Claim: *field.Value.String,
				DisplayName: address.Name, Ordinal: index, Role: role,
				Sensitive: field.Sensitive || field.Key == "email.bcc",
			}})
		}
	}
	return actors, nil
}

func metadataEmailActorRole(key string) (document.EventRole, bool) {
	switch key {
	case "email.from":
		return "sender", true
	case "email.to":
		return "recipient", true
	case "email.cc":
		return "copied", true
	case "email.bcc":
		return "blind_copy", true
	default:
		return "", false
	}
}

func attachMetadataActors(events []document.DocumentEventV1, actors []metadataActorClaim) {
	for _, claim := range actors {
		target := 0
		if strings.HasPrefix(claim.fieldKey, "email.") {
			for index := range events {
				if events[index].DateKind == "sent" && events[index].EvidenceKind == "source_metadata" {
					target = index
					break
				}
			}
		} else {
			for index := range events {
				if events[index].EvidenceKind == "source_metadata" {
					target = index
					break
				}
			}
		}
		events[target].Actors = append(events[target].Actors, claim.actor)
	}
}
