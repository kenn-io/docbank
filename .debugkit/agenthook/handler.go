package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

// Handler receives normalized, Claude-shaped hook events. Embed NoopHandler
// and override only the event methods an application needs.
type Handler interface {
	SessionStart(context.Context, SessionStartInput) (SessionStartOutput, error)
	UserPromptSubmit(context.Context, UserPromptSubmitInput) (UserPromptSubmitOutput, error)
	PreToolUse(context.Context, PreToolUseInput) (PreToolUseOutput, error)
	PostToolUse(context.Context, PostToolUseInput) (PostToolUseOutput, error)
	PostToolUseFailure(context.Context, PostToolUseFailureInput) (PostToolUseFailureOutput, error)
	PermissionRequest(context.Context, PermissionRequestInput) (PermissionRequestOutput, error)
	Notification(context.Context, NotificationInput) (NotificationOutput, error)
	Stop(context.Context, StopInput) (StopOutput, error)
	SessionEnd(context.Context, SessionEndInput) (SessionEndOutput, error)
}

// NoopHandler implements Handler without taking any action. Applications can
// embed it and override only the event methods they handle.
type NoopHandler struct{}

func (NoopHandler) SessionStart(context.Context, SessionStartInput) (SessionStartOutput, error) {
	return SessionStartOutput{}, nil
}

func (NoopHandler) UserPromptSubmit(
	context.Context,
	UserPromptSubmitInput,
) (UserPromptSubmitOutput, error) {
	return UserPromptSubmitOutput{}, nil
}

func (NoopHandler) PreToolUse(context.Context, PreToolUseInput) (PreToolUseOutput, error) {
	return PreToolUseOutput{}, nil
}

func (NoopHandler) PostToolUse(context.Context, PostToolUseInput) (PostToolUseOutput, error) {
	return PostToolUseOutput{}, nil
}

func (NoopHandler) PostToolUseFailure(
	context.Context,
	PostToolUseFailureInput,
) (PostToolUseFailureOutput, error) {
	return PostToolUseFailureOutput{}, nil
}

func (NoopHandler) PermissionRequest(
	context.Context,
	PermissionRequestInput,
) (PermissionRequestOutput, error) {
	return PermissionRequestOutput{}, nil
}

func (NoopHandler) Notification(
	context.Context,
	NotificationInput,
) (NotificationOutput, error) {
	return NotificationOutput{}, nil
}

func (NoopHandler) Stop(context.Context, StopInput) (StopOutput, error) {
	return StopOutput{}, nil
}

func (NoopHandler) SessionEnd(context.Context, SessionEndInput) (SessionEndOutput, error) {
	return SessionEndOutput{}, nil
}

// Handle normalizes one native agent payload, dispatches its Claude event to
// handler, and writes an agent-compatible hook response as JSON. Input must be
// one finite JSON object that reaches EOF; payloads larger than 16 MiB are
// rejected. Context cancellation governs handler execution, but cannot
// interrupt reads from an arbitrary io.Reader.
func Handle(
	ctx context.Context,
	agent Agent,
	input io.Reader,
	output io.Writer,
	handler Handler,
) error {
	if handler == nil {
		return errors.New("agent hook handler is required")
	}
	if output == nil {
		return errors.New("agent hook output writer is required")
	}
	spec, ok := profiles[agent]
	if !ok {
		return fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	payload, err := normalize(agent, input)
	if err != nil {
		return err
	}

	var envelope struct {
		HookEventName Event `json:"hook_event_name"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode normalized agent hook payload: %w", err)
	}
	if envelope.HookEventName == "" {
		return errors.New("normalized agent hook payload missing hook_event_name")
	}

	result, err := dispatch(ctx, handler, spec, envelope.HookEventName, payload)
	if err != nil {
		return fmt.Errorf("handle %s agent hook: %w", envelope.HookEventName, err)
	}
	response, err := encodeResponse(spec, envelope.HookEventName, result)
	if err != nil {
		return err
	}
	if _, err := output.Write(response); err != nil {
		return fmt.Errorf("encode %s agent hook output: %w", envelope.HookEventName, err)
	}
	return nil
}

type handledOutput struct {
	value any
}

func dispatch(
	ctx context.Context,
	handler Handler,
	spec profileSpec,
	event Event,
	payload []byte,
) (handledOutput, error) {
	switch event {
	case EventSessionStart:
		var input SessionStartInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.SessionStart(ctx, input)
		return handledOutput{value: output}, err
	case EventUserPromptSubmit:
		var input UserPromptSubmitInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.UserPromptSubmit(ctx, input)
		return handledOutput{value: output}, err
	case EventPreToolUse:
		var input PreToolUseInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.PreToolUse(ctx, input)
		return handledOutput{value: output}, err
	case EventPostToolUse:
		var input PostToolUseInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.PostToolUse(ctx, input)
		return handledOutput{value: output}, err
	case EventPostToolUseFailure:
		var input PostToolUseFailureInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.PostToolUseFailure(ctx, input)
		return handledOutput{value: output}, err
	case EventPermissionRequest:
		var input PermissionRequestInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.PermissionRequest(ctx, input)
		return handledOutput{value: output}, err
	case EventNotification:
		var input NotificationInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.Notification(ctx, input)
		return handledOutput{value: output}, err
	case EventStop:
		var input StopInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.Stop(ctx, input)
		return handledOutput{value: output}, err
	case EventSessionEnd:
		var input SessionEndInput
		if err := decodeTypedInput(spec, event, payload, &input.CommonInput, &input); err != nil {
			return handledOutput{}, err
		}
		output, err := handler.SessionEnd(ctx, input)
		return handledOutput{value: output}, err
	default:
		return handledOutput{}, fmt.Errorf("unsupported Claude hook event %q", event)
	}
}

func decodeTypedInput(
	spec profileSpec,
	event Event,
	payload []byte,
	common *CommonInput,
	target any,
) error {
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode typed agent hook input: %w", err)
	}
	common.Raw = append(common.Raw[:0], payload...)
	if common.SessionID == "" {
		return fmt.Errorf("%s input missing session_id", event)
	}
	return validateTypedInput(spec, event, target)
}

func validateTypedInput(spec profileSpec, event Event, input any) error {
	missingToolInput := func(name string, toolInput json.RawMessage) error {
		if name == "" {
			return fmt.Errorf("%s input missing tool_name", event)
		}
		if len(toolInput) == 0 || string(toolInput) == "null" {
			return fmt.Errorf("%s input missing tool_input", event)
		}
		return nil
	}
	switch value := input.(type) {
	case *SessionStartInput:
		return validateInputRequirement(
			spec, event, "source", spec.sessionSourceRequirement, value.Source == "",
		)
	case *UserPromptSubmitInput:
		return validateInputRequirement(spec, event, "prompt", spec.userPromptRequirement, value.Prompt == "")
	case *PreToolUseInput:
		return missingToolInput(value.ToolName, value.ToolInput)
	case *PostToolUseInput:
		if err := missingToolInput(value.ToolName, value.ToolInput); err != nil {
			return err
		}
		if len(value.ToolResponse) == 0 || string(value.ToolResponse) == "null" {
			return errors.New("PostToolUse input missing tool_response")
		}
	case *PostToolUseFailureInput:
		if err := missingToolInput(value.ToolName, value.ToolInput); err != nil {
			return err
		}
		if value.Error == "" {
			return errors.New("PostToolUseFailure input missing error")
		}
	case *PermissionRequestInput:
		return missingToolInput(value.ToolName, value.ToolInput)
	case *NotificationInput:
		if value.Message == "" {
			return errors.New("Notification input missing message")
		}
		if value.NotificationType == "" {
			return errors.New("Notification input missing notification_type")
		}
	case *SessionEndInput:
		return validateInputRequirement(
			spec, event, "reason", spec.sessionEndReasonRequirement, value.Reason == "",
		)
	}
	return nil
}

func validateInputRequirement(
	spec profileSpec,
	event Event,
	field string,
	requirement inputRequirement,
	missing bool,
) error {
	switch requirement {
	case inputOptional:
		return nil
	case inputRequired:
		if missing {
			return fmt.Errorf("%s input missing %s", event, field)
		}
		return nil
	default:
		return fmt.Errorf(
			"%s has no %s %s requirement",
			spec.profile.DisplayName, event, field,
		)
	}
}

func encodeResponse(spec profileSpec, event Event, output handledOutput) ([]byte, error) {
	if err := validateTypedOutput(event, output.value); err != nil {
		return nil, fmt.Errorf("encode %s %s output: %w", spec.profile.DisplayName, event, err)
	}
	var (
		response map[string]any
		err      error
	)
	switch spec.responseFormat {
	case responseClaude:
		response, err = encodeClaudeResponse(spec, event, output.value)
	case responseCopilot:
		response, err = encodeCopilotResponse(event, output.value)
	case responseCursor:
		response, err = encodeCursorResponse(event, output.value)
	case responseGemini:
		response, err = encodeGeminiResponse(event, output.value)
	case responseHermes:
		response, err = encodeHermesResponse(event, output.value)
	case responseObservational:
		if !isZeroOutput(output.value) {
			return nil, fmt.Errorf(
				"%s does not support %s control output", spec.profile.DisplayName, event,
			)
		}
		response = map[string]any{}
	default:
		err = errors.New("unsupported agent hook response format")
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s %s output: %w", spec.profile.DisplayName, event, err)
	}
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode %s agent hook output: %w", event, err)
	}
	return append(data, '\n'), nil
}

func validateTypedOutput(event Event, value any) error {
	switch output := value.(type) {
	case UserPromptSubmitOutput:
		return validateDecision(event, output.Decision, output.Reason)
	case PreToolUseOutput:
		switch output.PermissionDecision {
		case "", PermissionDecisionAllow, PermissionDecisionDeny,
			PermissionDecisionAsk, PermissionDecisionDefer:
		default:
			return fmt.Errorf(
				"%s output has invalid permission decision %q",
				event, output.PermissionDecision,
			)
		}
		if output.PermissionDecision == PermissionDecisionDeny &&
			output.PermissionDecisionReason == "" {
			return errors.New("deny output requires a reason")
		}
		if len(output.UpdatedInput) > 0 {
			if err := validateJSONObject(output.UpdatedInput); err != nil {
				return fmt.Errorf("PreToolUse updatedInput must be a JSON object: %w", err)
			}
		}
	case PostToolUseOutput:
		return validateDecision(event, output.Decision, output.Reason)
	case PermissionRequestOutput:
		if output.Decision == nil {
			return nil
		}
		switch output.Decision.Behavior {
		case PermissionBehaviorAllow, PermissionBehaviorDeny:
		default:
			return fmt.Errorf(
				"%s output has invalid behavior %q", event, output.Decision.Behavior,
			)
		}
		if len(output.Decision.UpdatedInput) > 0 {
			if err := validateJSONObject(output.Decision.UpdatedInput); err != nil {
				return fmt.Errorf(
					"PermissionRequest updatedInput must be a JSON object: %w", err,
				)
			}
		}
	case StopOutput:
		return validateDecision(event, output.Decision, output.Reason)
	}
	return nil
}

func validateDecision(event Event, decision Decision, reason string) error {
	switch decision {
	case "", DecisionAllow, DecisionDeny, DecisionBlock:
	default:
		return fmt.Errorf("%s output has invalid decision %q", event, decision)
	}
	if decision == DecisionBlock && reason == "" {
		return fmt.Errorf("%s block output requires a reason", event)
	}
	return nil
}

func isZeroOutput(output any) bool {
	return reflect.ValueOf(output).IsZero()
}
