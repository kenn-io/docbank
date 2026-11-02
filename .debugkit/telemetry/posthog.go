package telemetry

import "go.kenn.io/kit/telemetry/posthog"

// The PostHog reporter lives in go.kenn.io/kit/telemetry/posthog, which links
// no OpenTelemetry. These names keep existing importers building.

const (
	// Deprecated: use posthog.DefaultEndpoint.
	DefaultPostHogEndpoint = posthog.DefaultEndpoint
	// Deprecated: use posthog.GenericEnabledEnv.
	GenericTelemetryEnabledEnv = posthog.GenericEnabledEnv
)

// Deprecated: use posthog.ErrUnsupportedEvent.
var ErrUnsupportedTelemetryEvent = posthog.ErrUnsupportedEvent

type (
	// Deprecated: use posthog.PropertyFilter.
	TelemetryPropertyFilter = posthog.PropertyFilter
	// Deprecated: use posthog.AllowedProperty.
	AllowedTelemetryProperty = posthog.AllowedProperty
	// Deprecated: use posthog.Option.
	PostHogOption = posthog.Option
	// Deprecated: use posthog.Client.
	PostHogClient = posthog.Client
	// Deprecated: use posthog.Options.
	PostHogOptions = posthog.Options
	// Deprecated: use posthog.Reporter.
	PostHogReporter = posthog.Reporter
)

var (
	// Deprecated: use posthog.AllowProperty.
	AllowTelemetryProperty = posthog.AllowProperty
	// Deprecated: use posthog.WithAllowedEvent.
	WithAllowedEvent = posthog.WithAllowedEvent
	// Deprecated: use posthog.EnabledFromEnv.
	PostHogTelemetryEnabledFromEnv = posthog.EnabledFromEnv
	// Deprecated: use posthog.PrefixedEnabledEnv.
	PrefixedTelemetryEnabledEnv = posthog.PrefixedEnabledEnv
	// Deprecated: use posthog.DisableProcess.
	DisablePostHogTelemetry = posthog.DisableProcess
	// Deprecated: use posthog.ProcessDisabled.
	PostHogTelemetryDisabled = posthog.ProcessDisabled
	// Deprecated: use posthog.NewReporter.
	NewPostHogReporter = posthog.NewReporter
	// Deprecated: use posthog.DisabledReporter.
	DisabledPostHogReporter = posthog.DisabledReporter
	// Deprecated: use posthog.NewCaptureHandler.
	NewPostHogCaptureHandler = posthog.NewCaptureHandler
	// Deprecated: use posthog.AllowNumber.
	AllowTelemetryNumber = posthog.AllowNumber
	// Deprecated: use posthog.AllowBool.
	AllowTelemetryBool = posthog.AllowBool
	// Deprecated: use posthog.AllowStringValues.
	AllowTelemetryStringValues = posthog.AllowStringValues
)
