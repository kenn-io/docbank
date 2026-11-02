package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	phsdk "github.com/posthog/posthog-go"
)

const (
	// DefaultEndpoint is PostHog's US ingest endpoint.
	DefaultEndpoint = "https://us.i.posthog.com"
	// GenericEnabledEnv is the unprefixed telemetry opt-out variable.
	// See EnabledFromEnv for accepted values and vetoes.
	GenericEnabledEnv = "TELEMETRY_ENABLED"

	// ShutdownTimeout bounds how long Reporter.Close waits for queued events,
	// so a daemon without network exits instead of waiting through retries.
	ShutdownTimeout = 2 * time.Second

	// postHogInstallAgeProperty holds whole hours since Options.InstalledAt.
	postHogInstallAgeProperty = "install_age_hours"
)

// ErrUnsupportedEvent is returned when an event is not in a reporter's
// event allowlist.
var ErrUnsupportedEvent = errors.New("unsupported telemetry event")

// ErrInvalidProperty marks a missing or rejected event property.
var ErrInvalidProperty = errors.New("invalid telemetry property")

var postHogTelemetryDisabled atomic.Bool

// projectKey is the PostHog project API key used when Options.APIKey is
// empty. Source builds leave it empty, so they report nothing; release builds
// set it at link time:
//
//	-X go.kenn.io/kit/telemetry/posthog.projectKey=<key>
var projectKey string

// distribution names the channel that built this binary, such as homebrew,
// and is sent as the distribution property on every event. Source builds
// leave it empty; release builds set it at link time:
//
//	-X go.kenn.io/kit/telemetry/posthog.distribution=<channel>
var distribution string

var releaseVersion = regexp.MustCompile(`^v?[0-9]+[.][0-9]+[.][0-9]+$`)

// Remove the same SDK system properties on every OS.
var postHogSDKPropertyNames = map[string]struct{}{
	"$os": {}, "$os_version": {}, "$os_distro": {}, "$go_version": {},
}

// PropertyFilter validates and returns a safe event property value.
type PropertyFilter func(any) (any, bool)

// AllowedProperty configures one safe property for an allowed event.
type AllowedProperty struct {
	name     string
	filter   PropertyFilter
	required bool
}

// Option customizes a PostHog telemetry reporter.
type Option interface {
	applyPostHogOption(*postHogReporterConfig)
}

// Client is the daemon-facing telemetry reporter contract.
type Client interface {
	Capture(event string, properties map[string]any) error
	Close() error
	// Enabled reports whether events can be sent. Once it returns false it must
	// stay false; RunHeartbeat stops at the first disabled check.
	Enabled() bool
}

// Options configures a PostHog telemetry reporter.
type Options struct {
	// Logger receives SDK and daily-claim housekeeping logs. Nil uses slog.Default().
	Logger *slog.Logger
	// APIKey is the PostHog project API key. It is a public ingest identifier.
	// Empty uses the key set at link time; with neither, the reporter is
	// disabled. Kit's source never embeds a key.
	APIKey string
	// Endpoint defaults to DefaultEndpoint when empty.
	Endpoint string
	// Application is included on every event and cannot be overridden by Capture.
	Application string
	// EnvPrefix names the PREFIX_TELEMETRY_ENABLED opt-out variable.
	// NewReporter samples it and TELEMETRY_ENABLED once through Eligible.
	EnvPrefix string
	// DistinctID must be an anonymous stable installation or instance ID.
	DistinctID string
	// Version is the application version in plain vX.Y.Z or X.Y.Z format.
	// Reporting requires Eligible(EnvPrefix, Version) to return true.
	Version string
	Commit  string
	// Source defaults to "daemon" when empty.
	Source string
	// InstalledAt is when DistinctID was first created, usually persisted
	// beside it. When set, every event carries install_age_hours, the whole
	// hours from InstalledAt to the capture (0 when InstalledAt is in the
	// future), so reports can filter young installs such as sandboxes and test
	// harnesses. Zero omits the property.
	InstalledAt time.Time
}

// Reporter sanitizes and submits anonymous telemetry events to PostHog.
type Reporter struct {
	mu             sync.Mutex
	client         postHogEnqueueCloser
	logger         *slog.Logger
	distinctID     string
	version        string
	commit         string
	application    string
	source         string
	allowedEvents  map[string]map[string]PropertyFilter
	requiredEvents map[string]map[string]bool
	dailyEvents    map[string]dailyEvent
	enabled        bool
	now            func() time.Time
	installedAt    time.Time
	deliveries     *deliveryCallbacks
}

type postHogEnqueueCloser interface {
	Enqueue(phsdk.Message) error
	FlushWithContext(context.Context) error
	Close() error
}

type postHogClientFactory func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error)

type postHogReporterConfig struct {
	allowedEvents  map[string]map[string]PropertyFilter
	requiredEvents map[string]map[string]bool
	dailyEvents    map[string]dailyEvent
	now            func() time.Time
}

type postHogOptionFunc func(*postHogReporterConfig)

func (f postHogOptionFunc) applyPostHogOption(config *postHogReporterConfig) {
	f(config)
}

// AllowProperty creates an allowlisted property entry for WithAllowedEvent.
func AllowProperty(name string, filter PropertyFilter) AllowedProperty {
	return AllowedProperty{
		name:   strings.TrimSpace(name),
		filter: filter,
	}
}

// RequireProperty rejects an event when name is missing or its filter rejects it.
// Its declaration requires a nonblank name and nonnil filter.
func RequireProperty(name string, filter PropertyFilter) AllowedProperty {
	property := AllowProperty(name, filter)
	property.required = true
	return property
}

// WithAllowedEvent allows event and the listed sanitized properties.
// Requiredness survives later declarations; the latest valid filter applies.
func WithAllowedEvent(event string, properties ...AllowedProperty) Option {
	return postHogOptionFunc(func(config *postHogReporterConfig) {
		if config == nil {
			return
		}
		event = strings.TrimSpace(event)
		if event == "" {
			return
		}
		if config.allowedEvents == nil {
			config.allowedEvents = map[string]map[string]PropertyFilter{}
		}
		allowedProperties := config.allowedEvents[event]
		if allowedProperties == nil {
			allowedProperties = map[string]PropertyFilter{}
			config.allowedEvents[event] = allowedProperties
		}
		for _, property := range properties {
			if property.required {
				if config.requiredEvents == nil {
					config.requiredEvents = make(map[string]map[string]bool)
				}
				if config.requiredEvents[event] == nil {
					config.requiredEvents[event] = make(map[string]bool)
				}
				valid, declared := config.requiredEvents[event][property.name]
				config.requiredEvents[event][property.name] = property.name != "" && property.filter != nil && (!declared || valid)
			}
			if property.name == "" || property.filter == nil {
				continue
			}
			allowedProperties[property.name] = property.filter
		}
	})
}

// EnabledFromEnv reports whether telemetry is enabled for envPrefix.
// See Eligible for the full reporting decision, including the release version.
// Reporting defaults to on. An explicit 0, false, no, or off in TELEMETRY_ENABLED
// or PREFIX_TELEMETRY_ENABLED turns it off. Values are trimmed and case-insensitive.
// Process disable and any present CI turn reporting off. A present DO_NOT_TRACK
// turns reporting off unless its trimmed, case-insensitive value is 0 or false.
// NewReporter samples the environment once, while DisableProcess also stops
// reporting after construction.
func EnabledFromEnv(envPrefix string) bool {
	if ProcessDisabled() {
		return false
	}
	if _, present := os.LookupEnv("CI"); present {
		return false
	}
	if value, present := os.LookupEnv("DO_NOT_TRACK"); present {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "0", "false":
		default:
			return false
		}
	}
	for _, env := range []string{GenericEnabledEnv, PrefixedEnabledEnv(envPrefix)} {
		if env == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(os.Getenv(env))) {
		case "0", "false", "no", "off":
			return false
		}
	}
	return true
}

// Eligible reports whether the admission gate allows envPrefix and version at
// the time of the call. It reads the current environment and process-disable
// state each time. NewReporter samples the environment at construction, so a
// forwarding client should call Eligible before each send with its own prefix
// and version. The version must match plain vX.Y.Z or X.Y.Z numeric components
// exactly, without trimming.
func Eligible(envPrefix, version string) bool {
	return EnabledFromEnv(envPrefix) && releaseVersion.MatchString(version)
}

// PrefixedEnabledEnv returns the telemetry opt-out environment variable
// for prefix, such as KATA_TELEMETRY_ENABLED.
func PrefixedEnabledEnv(prefix string) string {
	prefix = strings.Trim(strings.ToUpper(strings.TrimSpace(prefix)), "_")
	if prefix == "" {
		return ""
	}
	return prefix + "_TELEMETRY_ENABLED"
}

// DisableProcess disables PostHog telemetry for this process.
//
// Callers can use this from their own build-tagged file, for example:
//
//	//go:build myapp_test
//	package myapp
//
//	import "go.kenn.io/kit/telemetry/posthog"
//
//	func init() {
//		posthog.DisableProcess()
//	}
func DisableProcess() {
	postHogTelemetryDisabled.Store(true)
}

// ProcessDisabled reports whether telemetry was disabled for this process.
func ProcessDisabled() bool {
	return postHogTelemetryDisabled.Load()
}

// NewReporter builds a reporter after sampling Eligible(opts.EnvPrefix, opts.Version)
// and requiring a project key, from Options.APIKey or set at link time.
// Disabled construction skips client configuration validation and client creation,
// preserving configured events for handler validation. Invalid event declarations
// still return errors.
// Later environment changes have no effect; DisableProcess stops reporting at runtime.
func NewReporter(opts Options, options ...Option) (*Reporter, error) {
	return newPostHogReporter(opts, func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error) {
		return phsdk.NewWithConfig(apiKey, config)
	}, options...)
}

func newPostHogReporter(opts Options, newClient postHogClientFactory, options ...Option) (*Reporter, error) {
	config := postHogReporterConfig{}
	for _, option := range options {
		if option != nil {
			option.applyPostHogOption(&config)
		}
	}
	for event, properties := range config.requiredEvents {
		for property, valid := range properties {
			if !valid {
				return nil, fmt.Errorf("required telemetry property %q for %q needs a nonblank name and nonnil filter", property, event)
			}
		}
	}
	allowedEvents := cloneAllowedTelemetryEvents(config.allowedEvents)
	defaults := make(map[string]any)
	(&Reporter{}).addDefaultProperties(defaults)
	for event, daily := range config.dailyEvents {
		properties, allowed := allowedEvents[event]
		if !allowed {
			return nil, fmt.Errorf("daily telemetry event %q is not allowed", event)
		}
		if properties[daily.key] == nil {
			return nil, fmt.Errorf("daily telemetry key %q is not allowed for %q", daily.key, event)
		}
		if _, owned := defaults[daily.key]; owned || daily.key == postHogInstallAgeProperty {
			return nil, fmt.Errorf("daily telemetry key %q is reporter-owned", daily.key)
		}
		if daily.claims == nil || strings.TrimSpace(daily.claims.path) == "" {
			return nil, errors.New("daily telemetry state path is required")
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "posthog")
	warned := make(map[string]bool)
	for _, properties := range allowedEvents {
		for key := range properties {
			if _, sdkOwned := postHogSDKPropertyNames[key]; sdkOwned && !warned[key] {
				logger.Warn("telemetry property is removed from uploads", "property", key)
				warned[key] = true
			}
		}
	}
	apiKey := defaultString(strings.TrimSpace(opts.APIKey), strings.TrimSpace(projectKey))
	if apiKey == "" || !Eligible(opts.EnvPrefix, opts.Version) {
		return &Reporter{allowedEvents: allowedEvents, requiredEvents: config.requiredEvents, dailyEvents: config.dailyEvents}, nil
	}
	if newClient == nil {
		return nil, errors.New("posthog client factory is required")
	}
	if strings.TrimSpace(opts.Application) == "" {
		return nil, errors.New("telemetry application is required")
	}
	if strings.TrimSpace(opts.EnvPrefix) == "" {
		return nil, errors.New("telemetry env prefix is required")
	}
	if strings.TrimSpace(opts.DistinctID) == "" {
		return nil, errors.New("telemetry distinct id is required")
	}

	if len(allowedEvents) == 0 {
		return nil, errors.New("telemetry allowed events are required")
	}

	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	disableGeoIP := true
	deliveries := &deliveryCallbacks{pending: make(map[chan error]deliveryKey)}
	client, err := newClient(apiKey, phsdk.Config{
		Callback:     deliveries,
		Logger:       sdkLogger{logger},
		Endpoint:     endpoint,
		DisableGeoIP: &disableGeoIP,
		// Reporters run in CLIs and daemons on user machines, not servers.
		IsServer: new(false),
		// The transport parses legacy, uncompressed batches.
		CaptureMode:     phsdk.CaptureModeLegacy,
		Transport:       postHogDisableTransport{allowedEvents: allowedEvents},
		ShutdownTimeout: ShutdownTimeout,
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:         client,
		logger:         logger,
		distinctID:     strings.TrimSpace(opts.DistinctID),
		version:        opts.Version,
		commit:         opts.Commit,
		application:    strings.TrimSpace(opts.Application),
		source:         defaultString(strings.TrimSpace(opts.Source), "daemon"),
		allowedEvents:  allowedEvents,
		requiredEvents: config.requiredEvents,
		dailyEvents:    config.dailyEvents,
		enabled:        true,
		now:            config.now,
		installedAt:    opts.InstalledAt,
		deliveries:     deliveries,
	}, nil
}

// DisabledReporter returns a reporter that drops events without network calls.
func DisabledReporter() *Reporter {
	return &Reporter{}
}

// Enabled reports whether the reporter can submit telemetry events.
func (r *Reporter) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked() && !ProcessDisabled()
}

// EventAllowed reports whether event is included in the reporter's allowlist.
func (r *Reporter) EventAllowed(event string) bool {
	if r == nil {
		return false
	}
	_, ok := r.allowedEvents[strings.TrimSpace(event)]
	return ok
}

// SanitizeProperties returns only allowlisted properties for event.
func (r *Reporter) SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, ErrUnsupportedEvent
	}
	allowedProperties, ok := r.allowedEvents[strings.TrimSpace(event)]
	if !ok {
		return nil, ErrUnsupportedEvent
	}

	safeProperties := map[string]any{}
	for key, value := range properties {
		key = strings.TrimSpace(key)
		filter, ok := allowedProperties[key]
		if !ok {
			continue
		}
		if safeValue, ok := filter(value); ok {
			safeProperties[key] = safeValue
		}
	}
	for key := range r.requiredEvents[strings.TrimSpace(event)] {
		if _, present := safeProperties[key]; !present {
			return nil, fmt.Errorf("%w: %s", ErrInvalidProperty, key)
		}
	}
	r.addDefaultProperties(safeProperties)
	return safeProperties, nil
}

// Capture sanitizes and queues an anonymous telemetry event. When
// Options.InstalledAt is set, the event carries install_age_hours.
func (r *Reporter) Capture(event string, properties map[string]any) error {
	if !r.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	_, err := r.Report(ctx, event, properties)
	return err
}

// Report validates and reports an event, returning its queue acceptance status.
func (r *Reporter) Report(ctx context.Context, event string, properties map[string]any) (Status, error) {
	event = strings.TrimSpace(event)
	props, err := r.SanitizeProperties(event, properties)
	if err != nil {
		return "", err
	}
	daily, limited := r.dailyEvents[event]
	var key string
	if limited {
		key, _ = props[daily.key].(string)
		if key == "" {
			return "", fmt.Errorf("%w: %s", ErrInvalidProperty, daily.key)
		}
	}
	if !r.Enabled() {
		return StatusDisabled, nil
	}
	if err := checkEncodable(props); err != nil {
		return "", err
	}
	send := func(now time.Time) (Status, error) {
		status, _, err := r.queue(ctx, event, props, now, "")
		return status, err
	}
	if limited {
		return daily.claims.report(ctx, r.logger, r.distinctID, event, key, r.clock, send)
	}
	return send(r.clock())
}

func checkEncodable(props map[string]any) error {
	if _, err := json.Marshal(props); err != nil {
		return fmt.Errorf("encode telemetry properties: %w", err)
	}
	return nil
}

func (r *Reporter) queue(ctx context.Context, event string, props map[string]any, now time.Time, id string) (Status, postHogEnqueueCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() || ProcessDisabled() {
		return StatusDisabled, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if !r.installedAt.IsZero() {
		// A future InstalledAt means the clock moved back; count the install as new.
		props[postHogInstallAgeProperty] = int64(max(now.Sub(r.installedAt), 0) / time.Hour)
	}
	err := r.client.Enqueue(phsdk.Capture{
		Uuid:       id,
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  now.UTC(),
		Properties: phsdk.Properties(props),
	})
	if err != nil {
		return "", nil, err
	}
	return StatusQueued, r.client, nil
}

// Close stops the underlying telemetry client when the reporter is enabled.
// It waits at most ShutdownTimeout for queued events and returns an error
// when it gives up on them. Reporter-created PostHog clients use a process-disable-aware transport, so
// Close can drain the SDK locally without network sends after telemetry is
// disabled for the process.
func (r *Reporter) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() {
		return nil
	}
	// The SDK refuses a second Close, so a timed-out reporter is done too.
	err := r.client.Close()
	r.deactivateLocked()
	r.deliveries.closeAll(phsdk.ErrClosed)
	return err
}

func (r *Reporter) activeLocked() bool {
	return r.enabled && r.client != nil
}

func (r *Reporter) deactivateLocked() {
	r.enabled = false
	r.client = nil
}

func (r *Reporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

type postHogDisableTransport struct {
	base          http.RoundTripper
	allowedEvents map[string]map[string]PropertyFilter
}

func (t postHogDisableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if ProcessDisabled() {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 " + http.StatusText(http.StatusNoContent),
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	if req.Method == http.MethodPost && req.Body != nil {
		defer req.Body.Close()
		body, err := t.rewriteBatch(req.Body)
		if err != nil {
			return nil, fmt.Errorf("filter telemetry batch: %w", err)
		}
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Del("Content-Length")
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// rewriteBatch filters after APIfy, which adds SDK properties after BeforeSend.
func (t postHogDisableTransport) rewriteBatch(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	envelope := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	batch, ok := envelope["batch"]
	if !ok {
		return data, nil
	}
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(batch, &events); err != nil {
		return nil, err
	}
	shared := make(map[string]any)
	(&Reporter{}).addDefaultProperties(shared)
	shared[postHogInstallAgeProperty], shared["$lib"], shared["$lib_version"] = nil, nil, nil
	for _, event := range events {
		if raw, present := event["properties"]; present {
			var name string
			if err := json.Unmarshal(event["event"], &name); err != nil {
				return nil, err
			}
			var properties map[string]json.RawMessage
			if err := json.Unmarshal(raw, &properties); err != nil {
				return nil, err
			}
			for key := range properties {
				_, sdkOwned := postHogSDKPropertyNames[key]
				if _, fixed := shared[key]; sdkOwned || (!fixed && t.allowedEvents[name][key] == nil) {
					delete(properties, key)
				}
			}
			if event["properties"], err = json.Marshal(properties); err != nil {
				return nil, err
			}
		}
		for key := range event {
			switch key {
			case "event", "distinct_id", "uuid", "timestamp", "properties":
			default:
				delete(event, key)
			}
		}
	}
	if envelope["batch"], err = json.Marshal(events); err != nil {
		return nil, err
	}
	for key := range envelope {
		if key != "api_key" && key != "batch" {
			delete(envelope, key)
		}
	}
	return json.Marshal(envelope)
}

func (r *Reporter) addDefaultProperties(props map[string]any) {
	props["$process_person_profile"] = false
	props["$geoip_disable"] = true
	props["application"] = r.application
	props["source"] = r.source
	props["version"] = r.version
	props["commit"] = r.commit
	props["distribution"] = distribution
	props["goos"] = runtime.GOOS
	props["goarch"] = runtime.GOARCH
	// The install age is reporter-owned; Capture sets it from InstalledAt.
	delete(props, postHogInstallAgeProperty)
}

// AllowNumber accepts finite numeric telemetry values.
func AllowNumber(value any) (any, bool) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}

// AllowBool accepts boolean telemetry values.
func AllowBool(value any) (any, bool) {
	v, ok := value.(bool)
	return v, ok
}

// AllowStringValues accepts only the listed string values.
func AllowStringValues(values ...string) PropertyFilter {
	allowed := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			allowed[value] = struct{}{}
		}
	}
	return func(value any) (any, bool) {
		v, ok := value.(string)
		if !ok {
			return nil, false
		}
		v = strings.TrimSpace(v)
		if _, ok := allowed[v]; !ok {
			return nil, false
		}
		return v, true
	}
}

func cloneAllowedTelemetryEvents(events map[string]map[string]PropertyFilter) map[string]map[string]PropertyFilter {
	cloned := make(map[string]map[string]PropertyFilter, len(events))
	for event, properties := range events {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		clonedProperties := map[string]PropertyFilter{}
		for property, filter := range properties {
			property = strings.TrimSpace(property)
			if property == "" || filter == nil {
				continue
			}
			clonedProperties[property] = filter
		}
		cloned[event] = clonedProperties
	}
	return cloned
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
