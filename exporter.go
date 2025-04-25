package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/oklog/run"
	arrowpb "github.com/open-telemetry/otel-arrow/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/pkg/otel/arrow_record"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

type Producer interface {
	Produce(pmetric.MetricSlice) error
	Collect(context.Context) error
}

type ProducerConfig struct {
	Producer  Producer
	ScopeName string
}

type stream struct {
	endpoint      arrowpb.ArrowMetricsService_ArrowMetricsClient
	arrowProducer *arrow_record.Producer
}

type Exporter struct {
	client arrowpb.ArrowMetricsServiceClient
	// NB: someday we might want to have several producer groups,
	// each of which collects at different intervals.
	// For now we are only collecting one scope (GPU metrics)
	// so the global interval is fine.
	interval      time.Duration
	producers     []ProducerConfig
	resourceAttrs map[string]any
	stream        *stream
}

func NewExporter(client arrowpb.ArrowMetricsServiceClient, interval time.Duration, resourceAttrs map[string]any) Exporter {
	return Exporter{
		client:        client,
		interval:      interval,
		resourceAttrs: resourceAttrs,
		stream:        nil,
	}
}

func (e *Exporter) AddProducer(p ProducerConfig) {
	e.producers = append(e.producers, p)
}

func (e *Exporter) makeStream(ctx context.Context) error {
	slog.Debug("making new stream")
	endpoint, err := e.client.ArrowMetrics(ctx)
	if err != nil {
		return err
	}
	p := arrow_record.NewProducer()
	e.stream = &stream{
		endpoint:      endpoint,
		arrowProducer: p,
	}
	return nil
}

func (e *Exporter) report(ctx context.Context) error {
	m := pmetric.NewMetrics()
	r := m.ResourceMetrics().AppendEmpty()
	if err := r.Resource().Attributes().FromRaw(e.resourceAttrs); err != nil {
		return err
	}
	for _, p := range e.producers {
		slog.Debug("Running arrow metrics producer", "scope", p.ScopeName)
		s := r.ScopeMetrics().AppendEmpty()
		s.Scope().SetName(p.ScopeName)
		ms := s.Metrics()
		if err := p.Producer.Produce(ms); err != nil {
			slog.Warn("Producer failed to produce metrics", "scope", p.ScopeName, "error", err)
		}
	}

	dpc := m.DataPointCount()
	slog.Debug("About to report arrow metrics", "data points", dpc)

	retriesRemaining := 1
	var err error
	var arrow *arrowpb.BatchArrowRecords
	for retriesRemaining >= 0 {
		retriesRemaining--
		if e.stream == nil {
			err = e.makeStream(ctx)
			if err != nil {
				// if we failed to create a new stream, don't retry.
				// The point of the retry loop is to handle the stream
				// legitimately going away e.g. due to the server
				// having specified max_connection_age,
				// not unexpected issues creating a new stream.
				break
			}
		}
		arrow, err = e.stream.arrowProducer.BatchArrowRecordsFromMetrics(m)
		if err != nil {
			slog.Warn("Error on produce", "error", err)
			e.stream = nil
			continue
		}
		start := time.Now()
		err = e.stream.endpoint.Send(arrow)
		if err != nil {
			slog.Warn("Error on send", "error", err)
			e.stream = nil
			continue
		}
		batchStatus, err := e.stream.endpoint.Recv()
		if err != nil {
			slog.Warn("Error on recv", "error", err)
			return err
		}
		if batchStatus.GetStatusCode() != arrowpb.StatusCode_OK {
			slog.Warn("unexpected status code",
				"status", batchStatus.GetStatusCode(),
				"message", batchStatus.GetStatusMessage(),
			)
			return err
		}

		slog.Info("Send succeeded",
			"data points", dpc,
			"duration", time.Since(start),
		)
		break
	}

	if err != nil {
		return err
	}

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
	slog.Info("running arrow metrics exporter", "producers", len(e.producers))
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
				return fmt.Errorf("failed to send arrow metrics: %v", err)
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
