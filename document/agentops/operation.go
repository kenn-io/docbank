// Package agentops defines the public, versioned authority and binding contract
// for daemon operations exposed to agents.
package agentops

const Schema = "docbank-agent/1"

type Class string

const (
	Read    Class = "read"
	Session Class = "session"
	Write   Class = "write"
	Admin   Class = "admin"
)

type Policy string

const (
	PolicyRead  Policy = "read"
	PolicyWrite Policy = "write"
	PolicyAdmin Policy = "admin"
)

func (p Policy) Allows(class Class) bool {
	switch p {
	case PolicyRead:
		return class == Read || class == Session
	case PolicyWrite:
		return class == Read || class == Session || class == Write
	case PolicyAdmin:
		return class == Read || class == Session || class == Write || class == Admin
	default:
		return false
	}
}

type Route struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	Pattern      string `json:"pattern"`
	Class        Class  `json:"class"`
	OperatorOnly bool   `json:"operator_only"`
}

type Bounds struct {
	Paging        string `json:"paging"`
	Default       int    `json:"default"`
	Maximum       int    `json:"maximum"`
	Allowed       []int  `json:"allowed,omitempty"`
	ResponseBytes int64  `json:"response_bytes"`
}

type SurfaceGap struct {
	Surface string `json:"surface"`
	Owner   string `json:"owner"`
	Reason  string `json:"reason"`
}

type JobSemantics struct {
	StatusRouteID string `json:"status_route_id,omitempty"`
	CancelRouteID string `json:"cancel_route_id,omitempty"`
	Durable       bool   `json:"durable"`
}

type ReceiptSemantics struct {
	Kind   string `json:"kind,omitempty"`
	Replay string `json:"replay,omitempty"`
}

type Operation struct {
	ID            string           `json:"id"`
	RouteIDs      []string         `json:"route_ids"`
	CLIPaths      []string         `json:"cli_paths"`
	MCPTools      []string         `json:"mcp_tools"`
	Feature       string           `json:"feature,omitempty"`
	Qualification string           `json:"qualification,omitempty"`
	Bounds        Bounds           `json:"bounds"`
	InputRef      string           `json:"input_ref"`
	OutputRef     string           `json:"output_ref"`
	Idempotent    bool             `json:"idempotent"`
	Destructive   bool             `json:"destructive"`
	OpenWorld     bool             `json:"open_world"`
	OperatorOnly  bool             `json:"operator_only"`
	Job           JobSemantics     `json:"job,omitzero"`
	Receipt       ReceiptSemantics `json:"receipt,omitzero"`
	SurfaceGaps   []SurfaceGap     `json:"surface_gaps,omitempty"`
}

// ReviewedBehavior is the publication gate for an operation descriptor.
// Zero-valued booleans never establish that a reviewer checked behavior.
func (operation Operation) ReviewedBehavior() bool {
	if operation.Qualification != "reviewed" || operation.Feature == "unqualified" ||
		operation.InputRef == "" || operation.OutputRef == "" || operation.Bounds.ResponseBytes <= 0 {
		return false
	}
	switch operation.Bounds.Paging {
	case "none":
		return operation.Bounds.Default == 0 && operation.Bounds.Maximum == 0 &&
			len(operation.Bounds.Allowed) == 0
	case "offset", "cursor":
		if operation.Bounds.Default < 1 || operation.Bounds.Maximum < operation.Bounds.Default {
			return false
		}
		for _, value := range operation.Bounds.Allowed {
			if value < 1 || value > operation.Bounds.Maximum {
				return false
			}
		}
		return true
	default:
		return false
	}
}
