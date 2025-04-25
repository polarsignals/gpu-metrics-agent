package main

import (
	"context"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Mock data extract from a real world PyTorch.
var (
	mockData = map[string][]int64{
		metricNameGPUPowerWatt:                {69, 69, 69, 73, 73, 73, 75, 75, 75, 69, 69, 69, 73, 73, 73, 46, 46, 46, 73, 73, 73, 74, 74, 74, 66, 66, 66, 72, 72, 72},
		metricNameGPUUtilizationMemoryPercent: {0, 57, 57, 53, 54, 53, 57, 13, 46, 55, 57, 53, 56, 54, 52, 12, 55, 54, 54, 55, 56, 54, 3, 30, 55, 56, 58, 55, 53, 0},
		metricNameGPUUtilizationPercent:       {3, 70, 70, 65, 67, 65, 70, 17, 57, 67, 70, 66, 69, 67, 64, 15, 68, 67, 66, 68, 69, 67, 4, 37, 67, 69, 72, 67, 65, 0},
	}
	mockDataIdle = map[string]int64{
		metricNameGPUPowerWatt:                6, // idle at ~6W
		metricNameGPUUtilizationMemoryPercent: 0,
		metricNameGPUUtilizationPercent:       0,
	}
)

type MockProducer struct {
	deviceLastTime map[string]time.Time
}

func (p *MockProducer) Collect(ctx context.Context) error {
	return nil
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
		slog.Info("Collecting metrics for device", attributeUUID, id, attributeIndex, i)

		lastTimeRounded := p.deviceLastTime[id].Truncate(PERIOD).Add(PERIOD)
		now := time.Now()

		for metricName, samples := range mockData {
			m := ms.AppendEmpty()
			g := m.SetEmptyGauge()

			m.SetName(metricName)

			// Create jitter based on the uuid so metrics values don't overlap.
			h := fnv.New32a()
			_, _ = h.Write([]byte(id))
			jitter := int64(h.Sum32() % 300)

			metricLastTime := lastTimeRounded

			for metricLastTime.Before(now) {
				mockDataIndex := (metricLastTime.Unix() - jitter) % 300
				var v int64
				if mockDataIndex < 30 {
					v = samples[mockDataIndex]
				} else {
					v = mockDataIdle[metricName]
				}

				dp := g.DataPoints().AppendEmpty()
				dp.Attributes().PutStr(attributeUUID, id)
				dp.Attributes().PutInt(attributeIndex, int64(i))
				dp.SetTimestamp(pcommon.NewTimestampFromTime(metricLastTime))
				dp.SetIntValue(v)

				metricLastTime = metricLastTime.Add(PERIOD)
			}
		}
		p.deviceLastTime[id] = now
	}

	return nil
}
