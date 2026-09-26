package agentops

import (
	"errors"
	"fmt"
)

type Feature struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type ServerCapabilities struct {
	VaultID        string      `json:"vault_id"`
	Version        string      `json:"version"`
	RegistryDigest string      `json:"registry_digest"`
	Routes         []Route     `json:"routes"`
	Operations     []Operation `json:"operations"`
	Features       []Feature   `json:"features"`
}

type Capabilities struct {
	Schema string             `json:"schema"`
	Server ServerCapabilities `json:"server"`
}

func (capabilities Capabilities) Validate() error {
	if capabilities.Schema != Schema {
		return fmt.Errorf("unsupported agent schema %q", capabilities.Schema)
	}
	for _, operation := range capabilities.Server.Operations {
		if !operation.ReviewedBehavior() {
			return fmt.Errorf("operation %q lacks reviewed behavioral metadata", operation.ID)
		}
	}
	seen := make(map[string]bool, len(capabilities.Server.Features))
	for _, feature := range capabilities.Server.Features {
		if feature.Name == "" || seen[feature.Name] {
			return errors.New("duplicate or empty feature")
		}
		seen[feature.Name] = true
		switch feature.State {
		case "available":
			if feature.Reason != "" {
				return errors.New("available feature has an unavailable reason")
			}
		case "unavailable", "unconfigured":
			if feature.Reason == "" {
				return fmt.Errorf("unavailable feature %q needs a reason", feature.Name)
			}
		default:
			return fmt.Errorf("unknown feature state %q", feature.State)
		}
	}
	return nil
}
