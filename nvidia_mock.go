package main

import (
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

type MockProducer struct {
	deviceLastTime map[string]time.Time
}

// NewNvidiaMockProducer creates a Producer that generates random data to send.
func NewNvidiaMockProducer(nDevices int, samplesFromTime time.Time) *MockProducer {
	deviceLastTime := make(map[string]time.Time, nDevices)
	for range nDevices {
		id := uuid.New().String()
		deviceLastTime[id] = samplesFromTime
	}

	return &MockProducer{deviceLastTime: deviceLastTime}
}

const PERIOD = time.Second / 6

func (p *MockProducer) Produce(ms pmetric.MetricSlice) error {
	deviceIDs := slices.Sorted(maps.Keys(p.deviceLastTime)) // Maps are unsorted, so we get its keys and sort

	for i, id := range deviceIDs {
		slog.Info("Collecting metrics for device", "uuid", id, "index", i)

		m := ms.AppendEmpty()
		g := m.SetEmptyGauge()

		now := time.Now()
		m.SetName("gpu_utilization_percent")

		// Create jitter based on the uuid so metrics values don't overlap.
		h := fnv.New32a()
		_, _ = h.Write([]byte(id))
		jitter := int64(h.Sum32() % 100)

		lastTimeRounded := p.deviceLastTime[id].Truncate(PERIOD).Add(PERIOD)

		for lastTimeRounded.Before(now) {
			// This will make the value go up and down between 0 and 100 based on the timestamp's seconds.
			v := (lastTimeRounded.Unix() - jitter) % 200
			if v > 100 {
				v = 200 - v
			}

			dp := g.DataPoints().AppendEmpty()
			dp.Attributes().PutStr("UUID", id)
			dp.Attributes().PutInt("index", int64(i))
			dp.SetTimestamp(pcommon.NewTimestampFromTime(lastTimeRounded))
			dp.SetIntValue(v)

			lastTimeRounded = lastTimeRounded.Add(PERIOD)
		}
		p.deviceLastTime[id] = now
	}

	return nil
}
