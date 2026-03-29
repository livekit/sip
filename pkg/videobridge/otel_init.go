// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package videobridge

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/livekit/sip/pkg/videobridge/config"
)

const tracerServiceName = "sip-video-bridge"

// TracerShutdown is a function that shuts down the tracer provider.
type TracerShutdown func(ctx context.Context) error

// InitTracer initializes OpenTelemetry tracing with OTLP gRPC exporter.
// Returns a shutdown function that must be called on process exit.
// If telemetry is disabled, returns a no-op shutdown.
func InitTracer(cfg config.TelemetryConfig, nodeID, version string) (TracerShutdown, error) {
	if !cfg.Enabled {
		return func(ctx context.Context) error { return nil }, nil
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("telemetry enabled but endpoint not configured")
	}

	ctx := context.Background()

	// Build gRPC dial options
	dialOpts := []grpc.DialOption{}
	if cfg.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Create OTLP exporter
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithDialOption(dialOpts...),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP exporter: %w", err)
	}

	// Build resource with service metadata
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(tracerServiceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(nodeID),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	// Configure sampler
	var sampler sdktrace.Sampler
	sampleRate := cfg.SampleRate
	if sampleRate <= 0 {
		sampleRate = 1.0
	}
	if sampleRate >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(sampleRate)
	}

	// Create tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set as global
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Update the package-level Tracer
	Tracer = tp.Tracer(tracerServiceName)

	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}

	return shutdown, nil
}
