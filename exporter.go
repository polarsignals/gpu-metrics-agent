package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/oklog/run"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"google.golang.org/grpc"
)

type Producer interface {
	Produce(pmetric.MetricSlice) error
	Collect(context.Context) error
}

type ProducerConfig struct {
	Producer  Producer
	ScopeName string
}

type Exporter struct {
	client pmetricotlp.GRPCClient
	// NB: someday we might want to have several producer groups,
	// each of which collects at different intervals.
	// For now we are only collecting one scope (GPU metrics)
	// so the global interval is fine.
	interval      time.Duration
	producers     []ProducerConfig
	resourceAttrs map[string]any
}

func NewExporter(conn *grpc.ClientConn, interval time.Duration, resourceAttrs map[string]any) Exporter {
	return Exporter{
		client:        pmetricotlp.NewGRPCClient(conn),
		interval:      interval,
		resourceAttrs: resourceAttrs,
	}
}

func (e *Exporter) AddProducer(p ProducerConfig) {
	e.producers = append(e.producers, p)
}

func (e *Exporter) report(ctx context.Context) error {
	m := pmetric.NewMetrics()
	r := m.ResourceMetrics().AppendEmpty()
	if err := r.Resource().Attributes().FromRaw(e.resourceAttrs); err != nil {
		return err
	}
	for _, p := range e.producers {
		slog.Debug("Running metrics producer", "scope", p.ScopeName)
		s := r.ScopeMetrics().AppendEmpty()
		s.Scope().SetName(p.ScopeName)
		ms := s.Metrics()
		if err := p.Producer.Produce(ms); err != nil {
			slog.Warn("Producer failed to produce metrics", "scope", p.ScopeName, "error", err)
		}
	}

	dpc := m.DataPointCount()
	slog.Debug("About to report otlp metrics", "data points", dpc)

	req := pmetricotlp.NewExportRequestFromMetrics(m)
	start := time.Now()
	resp, err := e.client.Export(ctx, req)
	if err != nil {
		return fmt.Errorf("otlp export failed: %w", err)
	}
	if ps := resp.PartialSuccess(); ps.RejectedDataPoints() > 0 || ps.ErrorMessage() != "" {
		slog.Warn("otlp partial success",
			"rejected", ps.RejectedDataPoints(),
			"message", ps.ErrorMessage(),
		)
	}

	slog.Info("Send succeeded",
		"data points", dpc,
		"duration", time.Since(start),
	)
	return nil
}

func (e *Exporter) Collect(ctx context.Context) error {
	var group run.Group

	for _, producer := range e.producers {
		group.Add(func() error {
			return producer.Producer.Collect(ctx)
		}, func(err error) {
			// TODO
		})
	}

	return group.Run()
}

func (e *Exporter) Start(ctx context.Context) error {
	slog.Info("running otlp metrics exporter", "producers", len(e.producers))
	if len(e.producers) == 0 {
		return errors.New("no producers configured")
	}
	tick := time.NewTicker(e.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := e.report(ctx); err != nil {
				return fmt.Errorf("failed to send otlp metrics: %v", err)
			}
			tick.Reset(addJitter(e.interval, 0.2))
		}
	}
}

// addJitter adds +/- jitter (jitter is [0..1]) to baseDuration
// originally copied from go.opentelemetry.io/epbf-profiler
func addJitter(baseDuration time.Duration, jitter float64) time.Duration {
	if jitter < 0.0 || jitter > 1.0 {
		return baseDuration
	}
	//nolint:gosec
	return time.Duration((1 + jitter - 2*jitter*rand.Float64()) * float64(baseDuration))
}
