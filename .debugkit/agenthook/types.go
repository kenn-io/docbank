package agenthook

import "encoding/json"

// CommonInput contains fields shared by Claude hook events. Raw contains the
// complete normalized payload for applications that need extension fields not
// represented by the typed event.
type CommonInput struct {
	SessionID      string          `json:"session_id"`
	PromptID       string          `json:"prompt_id,omitempty"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	PermissionMode PermissionMode  `json:"permission_mode,omitempty"`
	Effort         *Effort         `json:"effort,omitempty"`
	HookEventName  Event           `json:"hook_event_name"`
	AgentID        string          `json:"agent_id,omitempty"`
	AgentType      string          `json:"agent_type,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

// PermissionMode is the Claude Code permission policy active for an event.
type PermissionMode string

const (
	PermissionModeDefault           PermissionMode = "default"
	PermissionModePlan              PermissionMode = "plan"
	PermissionModeAcceptEdits       PermissionMode = "acceptEdits"
	PermissionModeAuto              PermissionMode = "auto"
	PermissionModeDontAsk           PermissionMode = "dontAsk"
	PermissionModeBypassPermissions PermissionMode = "bypassPermissions"
)

// Effort describes the model effort level active for an event.
type Effort struct {
	Level string `json:"level"`
}

// SessionStartInput is the typed Claude SessionStart payload.
type SessionStartInput struct {
	CommonInput
	// Source is empty when the native hook has no equivalent lifecycle concept.
	Source       SessionSource `json:"source"`
	Model        string        `json:"model,omitempty"`
	SessionTitle string        `json:"session_title,omitempty"`
}

// SessionSource identifies how a session started.
type SessionSource string

const (
	SessionSourceStartup SessionSource = "startup"
	SessionSourceResume  SessionSource = "resume"
	SessionSourceClear   SessionSource = "clear"
	SessionSourceCompact SessionSource = "compact"
	SessionSourceFork    SessionSource = "fork"
)

// UserPromptSubmitInput is the typed Claude UserPromptSubmit payload.
type UserPromptSubmitInput struct {
	CommonInput
	// Prompt is empty when the harness reports a running turn without its text, as OpenCode does.
	Prompt string `json:"prompt"`
}

// PreToolUseInput is the typed Claude PreToolUse payload.
type PreToolUseInput struct {
	CommonInput
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// PostToolUseInput is the typed Claude PostToolUse payload.
type PostToolUseInput struct {
	CommonInput
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	DurationMS   int64           `json:"duration_ms,omitempty"`
}

// PostToolUseFailureInput is the typed Claude PostToolUseFailure payload.
type PostToolUseFailureInput struct {
	CommonInput
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	Error       string          `json:"error"`
	IsInterrupt bool            `json:"is_interrupt,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
}

// PermissionRequestInput is the typed Claude PermissionRequest payload.
type PermissionRequestInput struct {
	CommonInput
	ToolName              string          `json:"tool_name"`
	ToolInput             json.RawMessage `json:"tool_input"`
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"`
}

// NotificationInput is the typed Claude Notification payload.
type NotificationInput struct {
	CommonInput
	Message          string           `json:"message"`
	Title            string           `json:"title,omitempty"`
	NotificationType NotificationType `json:"notification_type"`
}

// NotificationType identifies the kind of Claude notification.
type NotificationType string

const (
	NotificationPermissionPrompt    NotificationType = "permission_prompt"
	NotificationIdlePrompt          NotificationType = "idle_prompt"
	NotificationAuthSuccess         NotificationType = "auth_success"
	NotificationElicitationDialog   NotificationType = "elicitation_dialog"
	NotificationElicitationComplete NotificationType = "elicitation_complete"
	NotificationElicitationResponse NotificationType = "elicitation_response"
	NotificationAgentNeedsInput     NotificationType = "agent_needs_input"
	NotificationAgentCompleted      NotificationType = "agent_completed"
)

// StopInput is the typed Claude Stop payload.
type StopInput struct {
	CommonInput
	StopHookActive       bool             `json:"stop_hook_active,omitempty"`
	LastAssistantMessage string           `json:"last_assistant_message,omitempty"`
	BackgroundTasks      []BackgroundTask `json:"background_tasks,omitempty"`
	SessionCrons         []SessionCron    `json:"session_crons,omitempty"`
}

// BackgroundTask describes work that may resume a stopped Claude session.
type BackgroundTask struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Command     string `json:"command,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Server      string `json:"server,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Name        string `json:"name,omitempty"`
}

// SessionCron describes a scheduled session wakeup.
type SessionCron struct {
	ID        string `json:"id"`
	Schedule  string `json:"schedule"`
	Recurring bool   `json:"recurring"`
	Prompt    string `json:"prompt"`
}

// SessionEndInput is the typed Claude SessionEnd payload.
type SessionEndInput struct {
	CommonInput
	// Reason is empty when the native hook has no Claude-equivalent reason.
	Reason SessionEndReason `json:"reason"`
}

// SessionEndReason identifies why a Claude session terminated.
type SessionEndReason string

const (
	SessionEndClear                     SessionEndReason = "clear"
	SessionEndResume                    SessionEndReason = "resume"
	SessionEndLogout                    SessionEndReason = "logout"
	SessionEndPromptInputExit           SessionEndReason = "prompt_input_exit"
	SessionEndBypassPermissionsDisabled SessionEndReason = "bypass_permissions_disabled"
	SessionEndOther                     SessionEndReason = "other"
)

// CommonOutput contains control fields available to every Claude hook event.
type CommonOutput struct {
	Continue         *bool  `json:"continue,omitempty"`
	StopReason       string `json:"stopReason,omitempty"`
	SuppressOutput   bool   `json:"suppressOutput,omitempty"`
	SystemMessage    string `json:"systemMessage,omitempty"`
	TerminalSequence string `json:"terminalSequence,omitempty"`
}

// Decision is a top-level hook decision. Claude-shaped lifecycle controls use
// block; profiles whose native contract supports allow or deny retain those
// values at the encoder boundary.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionBlock Decision = "block"
)

// SessionStartOutput is the typed response from a SessionStart handler.
type SessionStartOutput struct {
	CommonOutput
	AdditionalContext  string
	InitialUserMessage string
	SessionTitle       string
	WatchPaths         []string
	ReloadSkills       bool
}

// UserPromptSubmitOutput is the typed response from a UserPromptSubmit handler.
type UserPromptSubmitOutput struct {
	CommonOutput
	Decision               Decision
	Reason                 string
	AdditionalContext      string
	SessionTitle           string
	SuppressOriginalPrompt bool
}

// PreToolUseOutput is the typed response from a PreToolUse handler.
type PreToolUseOutput struct {
	CommonOutput
	PermissionDecision       PermissionDecision
	PermissionDecisionReason string
	UpdatedInput             json.RawMessage
	AdditionalContext        string
}

// PostToolUseOutput is the typed response from a PostToolUse handler.
type PostToolUseOutput struct {
	CommonOutput
	Decision             Decision
	Reason               string
	AdditionalContext    string
	UpdatedToolOutput    json.RawMessage
	UpdatedMCPToolOutput json.RawMessage
}

// PostToolUseFailureOutput is the typed response from a PostToolUseFailure handler.
type PostToolUseFailureOutput struct {
	CommonOutput
	AdditionalContext string
}

// PermissionRequestOutput is the typed response from a PermissionRequest handler.
type PermissionRequestOutput struct {
	CommonOutput
	Decision *PermissionRequestDecision
}

// NotificationOutput is the typed response from a Notification handler.
type NotificationOutput struct {
	CommonOutput
}

// StopOutput is the typed response from a Stop handler.
type StopOutput struct {
	CommonOutput
	Decision          Decision
	Reason            string
	AdditionalContext string
}

// SessionEndOutput is the typed response from a SessionEnd handler.
type SessionEndOutput struct {
	CommonOutput
}

// PermissionDecision controls a Claude PreToolUse request.
type PermissionDecision string

const (
	PermissionDecisionAllow PermissionDecision = "allow"
	PermissionDecisionDeny  PermissionDecision = "deny"
	PermissionDecisionAsk   PermissionDecision = "ask"
	PermissionDecisionDefer PermissionDecision = "defer"
)

// PermissionRequestDecision controls a Claude permission prompt.
type PermissionRequestDecision struct {
	Behavior           PermissionBehavior `json:"behavior"`
	UpdatedInput       json.RawMessage    `json:"updatedInput,omitempty"`
	UpdatedPermissions json.RawMessage    `json:"updatedPermissions,omitempty"`
	Message            string             `json:"message,omitempty"`
	Interrupt          bool               `json:"interrupt,omitempty"`
}

// PermissionBehavior allows or denies a Claude permission prompt.
type PermissionBehavior string

const (
	PermissionBehaviorAllow PermissionBehavior = "allow"
	PermissionBehaviorDeny  PermissionBehavior = "deny"
)
