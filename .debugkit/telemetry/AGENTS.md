# Telemetry

This package holds the OpenTelemetry setup. The PostHog reporter lives in
`posthog`, which must never import OpenTelemetry; the PostHog names here are
deprecated aliases for existing importers.
