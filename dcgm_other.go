//go:build !linux

package main

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// DcgmProducer is a stub for non-Linux platforms. libdcgm is Linux-only.
type DcgmProducer struct{}

func NewDcgmProducer() (*DcgmProducer, error) {
	return nil, fmt.Errorf("DCGM profiling producer is only supported on Linux")
}

func (p *DcgmProducer) Close() {}

func (p *DcgmProducer) Collect(_ context.Context) error {
	return fmt.Errorf("DCGM profiling producer is only supported on Linux")
}

func (p *DcgmProducer) Produce(_ pmetric.MetricSlice) error {
	return nil
}
