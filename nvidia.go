package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/oklog/run"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	attributeIndex                        = "index"
	attributeUUID                         = "uuid"
	metricNameGPUUtilizationMemoryPercent = "gpu_utilization_memory_percent"
	metricNameGPUUtilizationPercent       = "gpu_utilization_percent"
	metricNameGPUPowerWatt                = "gpu_power_watt"
)

type perDeviceState struct {
	d nvml.Device
	//lastTimestamp map[string]uint64
}

type NvidiaProducer struct {
	devices []perDeviceState

	mu          sync.RWMutex
	metricSlice pmetric.MetricSlice
}

func NewNvidiaProducer() (*NvidiaProducer, error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML library: %s", nvml.ErrorString(ret))
	}
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get count of Nvidia devices: %s", nvml.ErrorString(ret))
	}
	devices := make([]perDeviceState, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get handle for Nvidia device %d: %s", i, nvml.ErrorString(ret))
		}
		devices[i] = perDeviceState{
			d: device,
			//lastTimestamp: map[string]uint64{
			//	metricNameGPUPowerWatt:                0,
			//	metricNameGPUUtilizationMemoryPercent: 0,
			//	metricNameGPUUtilizationPercent:       0,
			//},
		}
	}
	return &NvidiaProducer{
		devices: devices,

		mu:          sync.RWMutex{},
		metricSlice: pmetric.NewMetricSlice(),
	}, nil
}

func (p *NvidiaProducer) Collect(_ context.Context) (pmetric.MetricSlice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	collect := p.metricSlice

	// reset the metricSlice for new batches
	//p.metricSlice = pmetric.NewMetricSlice()

	return collect, nil
}

func (p *NvidiaProducer) Produce(ctx context.Context) error {
	var g run.Group

	for i, pds := range p.devices {
		uuid, ret := pds.d.GetUUID()
		if ret != nvml.SUCCESS {
			slog.Error("Failed to get device uuid", "index", i, "error", nvml.ErrorString(ret))
			continue
		}
		slog.Debug("Collecting metrics for device", "uuid", uuid, "index", i)

		{
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			g.Add(func() error {
				for {
					select {
					case <-ctx.Done():
						ticker.Stop()
						return nil
					case <-ticker.C:
						slog.Info("Collecting utilization metrics for device", "uuid", uuid, "index", i)
						m, err := produceUtilization(pds, uuid, i)
						if err != nil {
							return err
						}
						p.mu.Lock()
						ms := p.metricSlice.AppendEmpty()
						m.CopyTo(ms)
						p.mu.Unlock()
					}
				}

			}, func(err error) {
				ticker.Stop()
			})
		}
		{
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			g.Add(func() error {

				for {
					select {
					case <-ctx.Done():
						ticker.Stop()
						return nil
					case <-ticker.C:
						slog.Info("Collecting memory utilization metrics for device", "uuid", uuid, "index", i)
						m, err := produceMemoryUtilization(uuid, i)
						if err != nil {
							return err
						}
						p.mu.Lock()
						ms := p.metricSlice.AppendEmpty()
						m.CopyTo(ms)
						p.mu.Unlock()
					}
				}

			}, func(err error) {
				ticker.Stop()
			})
		}
		{
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			g.Add(func() error {
				for {
					select {
					case <-ctx.Done():
						ticker.Stop()
						return nil
					case <-ticker.C:
						slog.Info("Collecting power consumption metrics for device", "uuid", uuid, "index", i)
						m, err := producePowerConsumption(pds, uuid, i)
						if err != nil {
							return err
						}
						p.mu.Lock()
						ms := p.metricSlice.AppendEmpty()
						m.CopyTo(ms)
						p.mu.Unlock()
					}
				}

			}, func(err error) {
				ticker.Stop()
			})
		}
	}

	return g.Run()

	//for i, pds := range p.devices {
	//	uuid, ret := pds.d.GetUUID()
	//	if ret != nvml.SUCCESS {
	//		slog.Error("Failed to get device uuid", "index", i, "error", nvml.ErrorString(ret))
	//		continue
	//	}
	//	slog.Debug("Collecting metrics for device", "uuid", uuid, "index", i)
	//
	//	err := p.produceUtilization(pds, uuid, i, ms)
	//	if err != nil {
	//		slog.Error("Failed to get GPU utilization for device", "uuid", uuid, "index", i, "error", err)
	//		continue
	//	}
	//
	//	err = p.produceMemoryUtilization(pds, uuid, i, ms)
	//	if err != nil {
	//		slog.Error("Failed to get GPU memory utilization for device", "uuid", uuid, "index", i, "error", err)
	//		continue
	//	}
	//
	//	err = p.producePowerConsumption(pds, uuid, i, ms)
	//	if err != nil {
	//		slog.Error("Failed to get GPU memory utilization for device", "uuid", uuid, "index", i, "error", err)
	//		continue
	//	}
	//}
}

func produceUtilization(pds perDeviceState, uuid string, index int) (pmetric.Metric, error) {
	metricName := metricNameGPUUtilizationPercent

	m := pmetric.NewMetric()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.GPU_UTILIZATION_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return m, ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return m, err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		if s.TimeStamp == 0 {
			continue
		}

		pds.lastTimestamp[metricName] = max(pds.lastTimestamp[metricName], s.TimeStamp)

		value := getValue(s.SampleValue).(int64)

		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, uuid)
		dp.Attributes().PutInt(attributeIndex, int64(index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	return m, nil
}

func produceMemoryUtilization(uuid string, index int) (pmetric.Metric, error) {
	metricName := metricNameGPUUtilizationMemoryPercent

	m := pmetric.NewMetric()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.MEMORY_UTILIZATION_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return m, ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return m, err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		if s.TimeStamp == 0 {
			continue
		}

		pds.lastTimestamp[metricName] = max(pds.lastTimestamp[metricName], s.TimeStamp)

		value := getValue(s.SampleValue).(int64)
		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, uuid)
		dp.Attributes().PutInt(attributeIndex, int64(index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	return m, nil
}

func producePowerConsumption(pds perDeviceState, uuid string, index int) (pmetric.Metric, error) {
	metricName := metricNameGPUPowerWatt

	m := pmetric.NewMetric()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.TOTAL_POWER_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return m, ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return m, err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		if s.TimeStamp == 0 {
			continue
		}
		value := getValue(s.SampleValue).(int64) / 1000 // divide from milli watts to watts
		if value > 10*1000 {                            // ignore if above 10k watt
			continue
		}
		if value < 0 { // ignore negative power consumption
			continue
		}

		pds.lastTimestamp[metricName] = max(pds.lastTimestamp[metricName], s.TimeStamp)

		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, uuid)
		dp.Attributes().PutInt(attributeIndex, int64(index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	return m, nil
}

func valueGetter(sampleType nvml.ValueType) (func([8]byte) any, error) {
	switch sampleType {
	case nvml.VALUE_TYPE_DOUBLE:
		return func(val [8]byte) any {
			var value float64
			// TODO - test this on a big-endian machine
			err := binary.Read(bytes.NewReader(val[:]), binary.NativeEndian, &value)
			if err != nil {
				// justification for panic: this can never happen unless we've made
				// a programming error.
				panic(err)
			}
			return value
			// dp.SetDoubleValue(value)
		}, nil
	case nvml.VALUE_TYPE_UNSIGNED_INT, nvml.VALUE_TYPE_UNSIGNED_LONG, nvml.VALUE_TYPE_UNSIGNED_LONG_LONG, nvml.VALUE_TYPE_SIGNED_LONG_LONG, nvml.VALUE_TYPE_SIGNED_INT, nvml.VALUE_TYPE_COUNT:
		return func(val [8]byte) any {
			var value int64
			// TODO - test this on a big-endian machine
			err := binary.Read(bytes.NewReader(val[:]), binary.NativeEndian, &value)
			if err != nil {
				// justification for panic: this can never happen unless we've made
				// a programming error.
				panic(err)
			}
			return value
		}, nil
	default:
		return nil, fmt.Errorf("unsupported sample type %v", sampleType)
	}
}
