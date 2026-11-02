package telemetry

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestInitAppliesNativeOptionsAndRegistersGlobals(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_PROPAGATORS", "")

	spanRecorder := tracetest.NewSpanRecorder()
	metricReader := metric.NewManualReader()
	var tracerProvider oteltrace.TracerProvider
	var meterProvider otelmetric.MeterProvider
	var textMapPropagator propagation.TextMapPropagator
	shutdown, err := initWithDependencies(t.Context(), initDependencies{
		newSpanExporter: func(context.Context) (trace.SpanExporter, error) {
			return nil, nil
		},
		newMetricReader: func(context.Context) (metric.Reader, error) {
			return nil, nil
		},
		setTracerProvider: func(provider oteltrace.TracerProvider) {
			tracerProvider = provider
		},
		setMeterProvider: func(provider otelmetric.MeterProvider) {
			meterProvider = provider
		},
		setTextMapPropagator: func(propagator propagation.TextMapPropagator) {
			textMapPropagator = propagator
		},
	},
		WithResourceOptions(resource.WithAttributes(attribute.String("test.resource", "configured"))),
		WithTracerProviderOptions(trace.WithSpanProcessor(spanRecorder)),
		WithMeterProviderOptions(metric.WithReader(metricReader)),
		WithPropagators(testPropagator{}),
	)
	require.NoError(err)
	require.NotNil(shutdown)
	t.Cleanup(func() {
		assert.NoError(shutdown(context.WithoutCancel(t.Context())))
	})

	require.NotNil(tracerProvider)
	_, span := tracerProvider.Tracer("go.kenn.io/kit/telemetry/init_test").Start(t.Context(), "test")
	span.End()
	spans := spanRecorder.Ended()
	require.Len(spans, 1)
	value, ok := spans[0].Resource().Set().Value("test.resource")
	require.True(ok)
	assert.Equal("configured", value.AsString())

	require.NotNil(meterProvider)
	counter, err := meterProvider.Meter("go.kenn.io/kit/telemetry/init_test").Int64Counter("test.counter")
	require.NoError(err)
	counter.Add(t.Context(), 1)

	var metrics metricdata.ResourceMetrics
	require.NoError(metricReader.Collect(t.Context(), &metrics))
	require.NotEmpty(metrics.ScopeMetrics)
	require.NotNil(textMapPropagator)
	assert.Equal([]string{"test-propagator"}, textMapPropagator.Fields())
}

func TestNewResourceAppliesEnvironmentBeforeCallerOptions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.environment=loaded,test.precedence=environment")

	res, err := newResource(t.Context(),
		resource.WithAttributes(attribute.String("test.precedence", "caller")),
	)
	require.NoError(err)

	environmentValue, ok := res.Set().Value("test.environment")
	require.True(ok)
	assert.Equal("loaded", environmentValue.AsString())
	precedenceValue, ok := res.Set().Value("test.precedence")
	require.True(ok)
	assert.Equal("caller", precedenceValue.AsString())
}

func TestNewResourceDoesNotRetainRemovedEnvironmentAttributes(t *testing.T) {
	require := require.New(t)
	const helperEnv = "KIT_TEST_RESOURCE_ENV_REMOVAL"
	if os.Getenv(helperEnv) == "1" {
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.stale=present")
		_, err := newResource(t.Context())
		require.NoError(err)
		require.NoError(os.Unsetenv("OTEL_RESOURCE_ATTRIBUTES"))

		res, err := newResource(t.Context())
		require.NoError(err)
		_, ok := res.Set().Value("test.stale")
		assert.False(t, ok)
		return
	}

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestNewResourceDoesNotRetainRemovedEnvironmentAttributes$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	output, err := cmd.CombinedOutput()
	require.NoErrorf(err, "helper test failed:\n%s", output)
}

func TestNewResourceRetainsValidAttributesFromPartialEnvironment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.valid=value,invalid")

	res, err := newResource(t.Context())
	require.NoError(err)
	value, ok := res.Set().Value("test.valid")
	require.True(ok)
	assert.Equal("value", value.AsString())
}

func TestNewResourceReturnsCallerDetectorErrorWithPartialEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "test.valid=value,invalid")
	fatalErr := errors.New("caller detector failed")
	detector := resource.StringDetector("", "test.detector", func() (string, error) {
		return "", fatalErr
	})

	_, err := newResource(t.Context(), resource.WithDetectors(detector))
	require.ErrorIs(t, err, fatalErr)
}

func TestNewResourcePreservesServiceInstanceFeatureGate(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("OTEL_GO_X_RESOURCE", "")
		res, err := newResource(t.Context())
		require.NoError(t, err)
		_, ok := res.Set().Value("service.instance.id")
		assert.False(t, ok)
	})

	t.Run("enabled", func(t *testing.T) {
		t.Setenv("OTEL_GO_X_RESOURCE", "true")
		res, err := newResource(t.Context())
		require.NoError(t, err)
		_, ok := res.Set().Value("service.instance.id")
		assert.True(t, ok)
	})
}

func TestDefaultExporterFactoriesDisableEmptySelectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "")

	spanExporter, err := newSpanExporterFromEnv(t.Context())
	require.NoError(err)
	assert.Nil(spanExporter)

	metricReader, err := newMetricReaderFromEnv(t.Context())
	require.NoError(err)
	assert.Nil(metricReader)
}

func TestDefaultExporterFactoriesDisableExplicitNone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	spanExporter, err := newSpanExporterFromEnv(t.Context())
	require.NoError(err)
	assert.Nil(spanExporter)

	metricReader, err := newMetricReaderFromEnv(t.Context())
	require.NoError(err)
	assert.Nil(metricReader)
}

func TestInitCleansUpSpanExporterWhenMetricInitializationFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	initErr := errors.New("metric initialization failed")
	cleanupErr := errors.New("span exporter cleanup failed")
	exporter := &testSpanExporter{shutdownErr: cleanupErr}
	registrations := 0

	shutdown, err := initWithDependencies(t.Context(), initDependencies{
		newSpanExporter: func(context.Context) (trace.SpanExporter, error) {
			return exporter, nil
		},
		newMetricReader: func(context.Context) (metric.Reader, error) {
			return nil, initErr
		},
		setTracerProvider: func(oteltrace.TracerProvider) {
			registrations++
		},
		setMeterProvider: func(otelmetric.MeterProvider) {
			registrations++
		},
		setTextMapPropagator: func(propagation.TextMapPropagator) {
			registrations++
		},
	})

	assert.Nil(shutdown)
	require.ErrorIs(err, initErr)
	require.ErrorIs(err, cleanupErr)
	assert.Equal(1, exporter.shutdownCalls)
	assert.Zero(registrations)
}

func TestShutdownAllAttemptsEveryFunctionAndJoinsErrors(t *testing.T) {
	meterErr := errors.New("meter shutdown failed")
	tracerErr := errors.New("tracer shutdown failed")
	calls := []string{}

	err := shutdownAll(t.Context(),
		func(context.Context) error {
			calls = append(calls, "meter")
			return meterErr
		},
		func(context.Context) error {
			calls = append(calls, "tracer")
			return tracerErr
		},
	)

	assert.Equal(t, []string{"meter", "tracer"}, calls)
	require.ErrorIs(t, err, meterErr)
	require.ErrorIs(t, err, tracerErr)
}

type testPropagator struct {
	propagation.TraceContext
}

func (testPropagator) Fields() []string {
	return []string{"test-propagator"}
}

type testSpanExporter struct {
	shutdownCalls int
	shutdownErr   error
}

func (*testSpanExporter) ExportSpans(context.Context, []trace.ReadOnlySpan) error {
	return nil
}

func (e *testSpanExporter) Shutdown(context.Context) error {
	e.shutdownCalls++
	return e.shutdownErr
}
