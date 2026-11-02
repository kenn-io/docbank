//go:build kit_posthog_disabled

package posthog

func init() {
	DisableProcess()
}
