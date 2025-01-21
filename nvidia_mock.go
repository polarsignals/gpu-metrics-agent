package main

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/google/uuid"
)

type MockProducer struct {
	deviceUuids []string
	lastTime    time.Time
}

// NewNvidiaMockProducer creates a Producer that generates random data to send.
func NewNvidiaMockProducer(nDevices int, samplesFromTime time.Time) *MockProducer {
	deviceUuids := make([]string, 0, nDevices)
	for range nDevices {
		deviceUuids = append(deviceUuids, uuid.New().String())
	}

	return &MockProducer{
		deviceUuids: deviceUuids,
		lastTime:    samplesFromTime,
	}
}

const PERIOD = time.Second / 6

func (p *MockProducer) Produce(ms pmetric.MetricSlice) error {
	for i, uuid := range p.deviceUuids {
		slog.Debug("Collecting metrics for device", "uuid", uuid, "index", i)

		m := ms.AppendEmpty()
		g := m.SetEmptyGauge()

		now := time.Now()
		m.SetName("gpu_utilization_percent")

		lastTimeRounded := p.lastTime.Truncate(PERIOD).Add(PERIOD)

		for lastTimeRounded.Before(now) {
			// This will make the value go up and down between 0 and 100 based on the timestamp's seconds.
			v := lastTimeRounded.Unix() % 200
			if v > 100 {
				v = 200 - v
			}

			dp := g.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("UUID", uuid)
			dp.Attributes().PutInt("index", int64(i))
			dp.SetTimestamp(pcommon.NewTimestampFromTime(lastTimeRounded))
			dp.SetIntValue(v)

			lastTimeRounded = lastTimeRounded.Add(PERIOD)
		}
		p.lastTime = now
	}

	return nil
}
