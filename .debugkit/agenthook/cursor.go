package agenthook

func cursorProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentCursor, DisplayName: "Cursor",
			ConfigFilename: "hooks.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventStop,
				EventSessionEnd,
			},
		},
		formatDirectJSON,
		"Shell",
		func() (string, error) { return userDotDir(".cursor") },
	)
	spec.eventName = cursorEventName
	// Cursor preToolUse and beforeSubmitPrompt have native control responses,
	// and failures otherwise allow the operation through by default:
	// https://cursor.com/docs/hooks.md#pretooluse
	// https://cursor.com/docs/hooks.md#beforesubmitprompt
	// https://cursor.com/docs/hooks.md#per-script-configuration-options
	spec.responseFormat = responseCursor
	spec.failClosedEvents = []Event{EventUserPromptSubmit, EventPreToolUse}
	spec.requireVersion = true
	// Cursor sessionStart omits source, while sessionEnd requires reason:
	// https://cursor.com/docs/hooks.md#sessionstart
	// https://cursor.com/docs/hooks.md#sessionend
	spec.sessionSourceRequirement = inputOptional
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

func cursorEventName(event Event) string {
	switch event {
	case EventSessionStart:
		return "sessionStart"
	case EventUserPromptSubmit:
		return "beforeSubmitPrompt"
	case EventPreToolUse:
		return "preToolUse"
	case EventPostToolUse:
		return "postToolUse"
	case EventPostToolUseFailure:
		return "postToolUseFailure"
	case EventStop:
		return "stop"
	case EventSessionEnd:
		return "sessionEnd"
	default:
		return string(event)
	}
}
