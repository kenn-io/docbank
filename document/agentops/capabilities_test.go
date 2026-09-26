package agentops

import "testing"

func TestAgentCapabilitiesRequireHonestUnavailableReason(t *testing.T) {
	capabilities := Capabilities{Schema: Schema, Server: ServerCapabilities{
		Features: []Feature{{Name: "timeline", State: "unavailable"}},
	}}
	if capabilities.Validate() == nil {
		t.Fatal("missing reason accepted")
	}
	capabilities.Server.Features[0].Reason = "timeline worker is not installed"
	if err := capabilities.Validate(); err != nil {
		t.Fatal(err)
	}
	capabilities.Server.Features[0].State = "ready"
	if capabilities.Validate() == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestAgentCapabilitiesRejectUnreviewedOperationMetadata(t *testing.T) {
	capabilities := Capabilities{Schema: Schema, Server: ServerCapabilities{
		Operations: []Operation{{ID: "synthetic", Qualification: "reviewed"}},
	}}
	if capabilities.Validate() == nil {
		t.Fatal("incomplete reviewed operation was advertised")
	}
}
