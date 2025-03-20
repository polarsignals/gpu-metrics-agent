package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"log/slog"
	"sort"
)

const (
	attributeIndex                        = "index"
	attributeUUID                         = "uuid"
	metricNameGPUUtilizationMemoryPercent = "gpu_utilization_memory_percent"
	metricNameGPUUtilizationPercent       = "gpu_utilization_percent"
	metricNameGPUPowerWatt                = "gpu_power_watt"
)

type perDeviceState struct {
	d             nvml.Device
	lastTimestamp map[string]uint64
}

type producer struct {
	devices []perDeviceState
}

func NewNvidiaProducer() (*producer, error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML library: %v", nvml.ErrorString(ret))
	}
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get count of Nvidia devices: %v", nvml.ErrorString(ret))
	}
	devices := make([]perDeviceState, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("failed to get handle for Nvidia device %d: %v", i, nvml.ErrorString(ret))
		}
		devices[i] = perDeviceState{
			d: device,
			lastTimestamp: map[string]uint64{
				metricNameGPUPowerWatt:                0,
				metricNameGPUUtilizationMemoryPercent: 0,
				metricNameGPUUtilizationPercent:       0,
			},
		}
	}
	return &producer{
		devices: devices,
	}, nil
}

func (p *producer) Produce(ms pmetric.MetricSlice) error {
	for i, pds := range p.devices {
		uuid, ret := pds.d.GetUUID()
		if ret != nvml.SUCCESS {
			slog.Error("Failed to get device uuid", "index", i, "error", nvml.ErrorString(ret))
			continue
		}
		slog.Debug("Collecting metrics for device", "uuid", uuid, "index", i)

		err := p.produceUtilization(pds, uuid, i, ms)
		if err != nil {
			slog.Error("Failed to get GPU utilization for device", "uuid", uuid, "index", i, "error", err)
			continue
		}

		err = p.produceMemoryUtilization(pds, uuid, i, ms)
		if err != nil {
			slog.Error("Failed to get GPU memory utilization for device", "uuid", uuid, "index", i, "error", err)
			continue
		}

		err = p.producePowerConsumption(pds, uuid, i, ms)
		if err != nil {
			slog.Error("Failed to get GPU memory utilization for device", "uuid", uuid, "index", i, "error", err)
			continue
		}
	}

	return nil
}

func (p *producer) produceUtilization(pds perDeviceState, uuid string, index int, ms pmetric.MetricSlice) error {
	metricName := metricNameGPUUtilizationPercent

	m := ms.AppendEmpty()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.GPU_UTILIZATION_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return err
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

	return nil
}

func (p *producer) produceMemoryUtilization(pds perDeviceState, uuid string, index int, ms pmetric.MetricSlice) error {
	metricName := metricNameGPUUtilizationMemoryPercent

	m := ms.AppendEmpty()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.MEMORY_UTILIZATION_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return err
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

	return nil
}

func (p *producer) producePowerConsumption(pds perDeviceState, uuid string, index int, ms pmetric.MetricSlice) error {
	metricName := metricNameGPUPowerWatt

	m := ms.AppendEmpty()
	g := m.SetEmptyGauge()
	m.SetName(metricName)

	sampleType, samples, ret := pds.d.GetSamples(nvml.TOTAL_POWER_SAMPLES, pds.lastTimestamp[metricName])
	if !errors.Is(ret, nvml.SUCCESS) {
		return ret
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		if s.TimeStamp == 0 {
			continue
		}
		value := getValue(s.SampleValue).(int64)
		if value > 10000*1000 { // ignore if above 10k watt
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

	return nil
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
