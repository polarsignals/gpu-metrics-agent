package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/oklog/run"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	attributeClock                        = "clock"
	attributeIndex                        = "index"
	attributeUUID                         = "uuid"
	attributePID                          = "pid"
	attributeComm                         = "comm"
	metricNameGPUClockHertz               = "gpu_clock_hertz"
	metricNameGPUPCIeThroughputCount      = "gpu_pcie_throughput_count"
	metricNameGPUPCIeThroughputReceive    = "gpu_pcie_throughput_receive_bytes"
	metricNameGPUPCIeThroughputTransmit   = "gpu_pcie_throughput_transmit_bytes"
	metricNameGPUPowerLimitWatt           = "gpu_power_limit_watt"
	metricNameGPUPowerWatt                = "gpu_power_watt"
	metricNameGPUTemperatureCelsius       = "gpu_temperature_celsius"
	metricNameGPUUtilizationMemoryPercent = "gpu_utilization_memory_percent"
	metricNameGPUUtilizationPercent       = "gpu_utilization_percent"
)

// This file implements NVIDIA GPU metrics collection and production:
// 1. Collecting:
//    - Runs 3 concurrent collection loops with different intervals:
//      a) GPU and Memory Utilization (every 5s)
//      b) Power Consumption (every 1s)
//      c) PCIe Throughput (every 100ms - 10 times per second)
//    - Each loop collects metrics from all available NVIDIA devices
//    - Metrics are appended to a per-device per-metric gauge, and the last timestamp is stored too.
// 2. Producing:
//    - When Produce() is called, metrics from all devices are moved to the provided MetricSlice
//    - Each metric includes device UUID and index as attributes
//    - The producer maintains thread-safety using mutex locks
//    - After producing, the internal metric storage is cleared

type NvidiaProducer struct {
	devices []*perDeviceState
}

func NewNvidiaProducer() (*NvidiaProducer, error) {
	ret := nvml.Init()
	if !errors.Is(ret, nvml.SUCCESS) {
		return nil, fmt.Errorf("failed to initialize NVML library: %s", nvml.ErrorString(ret))
	}
	count, ret := nvml.DeviceGetCount()
	if !errors.Is(ret, nvml.SUCCESS) {
		return nil, fmt.Errorf("failed to get count of Nvidia devices: %s", nvml.ErrorString(ret))
	}
	devices := make([]*perDeviceState, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if !errors.Is(ret, nvml.SUCCESS) {
			return nil, fmt.Errorf("failed to get handle for Nvidia device %d: %s", i, nvml.ErrorString(ret))
		}
		uuid, ret := device.GetUUID()
		if !errors.Is(ret, nvml.SUCCESS) {
			return nil, fmt.Errorf("failed to get UUID for Nvidia device %d: %s", i, nvml.ErrorString(ret))
		}
		powerLimit, ret := nvml.DeviceGetPowerManagementLimit(device)
		if !errors.Is(ret, nvml.SUCCESS) {
			// Not supported on DGX
			if errors.Is(ret, nvml.ERROR_NOT_SUPPORTED) {
				slog.Warn("power limit not supported", "device", i, "err", nvml.ErrorString(ret))
			} else {
				return nil, fmt.Errorf("failed to get power limit for Nvidia device %d: %s", i, nvml.ErrorString(ret))
			}
		}

		devices[i] = &perDeviceState{
			d:          device,
			uuid:       uuid,
			index:      i,
			powerLimit: powerLimit,

			mu: &sync.RWMutex{},
			lastTimestamp: map[string]uint64{
				metricNameGPUPowerWatt:                0,
				metricNameGPUUtilizationMemoryPercent: 0,
				metricNameGPUUtilizationPercent:       0,
				metricNameGPUPowerLimitWatt:           0,
			},
			gauges: map[string]pmetric.Gauge{},
		}
	}
	return &NvidiaProducer{
		devices: devices,
	}, nil
}

func (p *NvidiaProducer) Collect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)

	var group run.Group

	{
		ticker := time.NewTicker(5 * time.Second)

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					} else {
						continue
					}
				case <-ticker.C:
					for _, pds := range p.devices {
						// We could consider making these concurrent,
						// but in reality we only call them every 5s,
						// so it's not worth it.
						if err := pds.collectUtilization(); err != nil {
							return err
						}
						if err := pds.collectMemoryUtilization(); err != nil {
							return err
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		ticker := time.NewTicker(1 * time.Second)

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					} else {
						continue
					}
				case <-ticker.C:
					for _, pds := range p.devices {
						if err := pds.collectProcessUtilization(); err != nil {
							return err
						}
					}

				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		ticker := time.NewTicker(1 * time.Second)

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					} else {
						continue
					}
				case <-ticker.C:
					for _, pds := range p.devices {
						if err := pds.collectClock(); err != nil {
							return err
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		ticker := time.NewTicker(time.Second)

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					}
					return nil
				case <-ticker.C:
					for _, pds := range p.devices {
						err := pds.collectPowerConsumption()
						if err != nil {
							return err
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		ticker := time.NewTicker(time.Second / 10) // 10x per second

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					}
					return nil
				case <-ticker.C:
					for _, pds := range p.devices {
						err := pds.collectPCIThroughput()
						if err != nil {
							return err
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}
	{
		ticker := time.NewTicker(time.Second)

		group.Add(func() error {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					if err := ctx.Err(); err != nil {
						return err
					}
					return nil
				case <-ticker.C:
					for _, pds := range p.devices {
						err := pds.collectTemperature()
						if err != nil {
							return err
						}
					}
				}
			}
		}, func(err error) {
			cancel()
		})
	}

	return group.Run()
}

func (p *NvidiaProducer) Produce(ms pmetric.MetricSlice) error {
	for _, pds := range p.devices {
		slog.Debug("Producing metrics for device",
			"uuid", pds.uuid,
			"index", pds.index,
		)

		for _, device := range p.devices {
			device.mu.Lock()

			for metricName, gauge := range device.gauges {
				m := ms.AppendEmpty()
				m.SetName(metricName)
				m.SetEmptyGauge()

				if gauge.DataPoints().Len() > 0 {
					slog.Debug("producing metric",
						"metric", metricName,
						"data points", gauge.DataPoints().Len(),
					)
					gauge.MoveTo(m.Gauge())
				}
			}

			device.mu.Unlock()

			// Append static metrics that were read at the beginning and never change.

			// Append power limit metric
			m := ms.AppendEmpty()
			m.SetName(metricNameGPUPowerLimitWatt)
			m.SetEmptyGauge()
			dp := m.Gauge().DataPoints().AppendEmpty()
			dp.Attributes().PutStr(attributeUUID, device.uuid)
			dp.Attributes().PutInt(attributeIndex, int64(device.index))
			dp.SetTimestamp(pcommon.Timestamp(time.Now().UnixNano()))
			dp.SetIntValue(int64((device.powerLimit) / 1000)) // Convert from milliwatts to watts

			slog.Debug("producing metric",
				"metric", metricNameGPUPowerLimitWatt,
				"data points", m.Gauge().DataPoints().Len(),
			)
		}
	}

	return nil
}

type perDeviceState struct {
	d          nvml.Device
	uuid       string
	index      int
	powerLimit uint32

	mu            *sync.RWMutex
	lastTimestamp map[string]uint64
	gauges        map[string]pmetric.Gauge
}

func (ds *perDeviceState) getLastTimestamp(metric string) uint64 {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.lastTimestamp[metric]
}

func (ds *perDeviceState) appendGauge(metricName string, maxTimestamp uint64, g pmetric.Gauge) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	slog.Debug("appending data points",
		"data points", g.DataPoints().Len(),
		"metric", metricName,
	)

	ds.lastTimestamp[metricName] = maxTimestamp
	if _, found := ds.gauges[metricName]; found {
		g.DataPoints().MoveAndAppendTo(ds.gauges[metricName].DataPoints())
	} else {
		ds.gauges[metricName] = g
	}
}

func (ds *perDeviceState) appendGaugeWithoutTime(metricName string, g pmetric.Gauge) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	slog.Debug("appending data points without max timestamp",
		"data points", g.DataPoints().Len(),
		"metric", metricName,
	)

	if _, found := ds.gauges[metricName]; found {
		g.DataPoints().MoveAndAppendTo(ds.gauges[metricName].DataPoints())
	} else {
		ds.gauges[metricName] = g
	}
}

func (ds *perDeviceState) collectUtilization() error {
	metricName := metricNameGPUUtilizationPercent
	g := pmetric.NewGauge()

	maxTimestamp := ds.getLastTimestamp(metricName)

	sampleType, samples, ret := ds.d.GetSamples(nvml.GPU_UTILIZATION_SAMPLES, maxTimestamp)
	if !errors.Is(ret, nvml.SUCCESS) {
		if errors.Is(ret, nvml.ERROR_NOT_FOUND) {
			slog.Warn("get GPU_UTILIZATION_SAMPLES returned not found", "err", ret)
			return nil
		}
		return fmt.Errorf("failed to get GPU_UTILIZATION_SAMPLES: %w", ret)
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		value := getValue(s.SampleValue).(int64)

		if s.TimeStamp == 0 {
			continue
		}
		if value < 0 || value > 100 { // ignore if below 0% or above 100%
			continue
		}

		maxTimestamp = max(maxTimestamp, s.TimeStamp)

		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	ds.appendGauge(metricName, maxTimestamp, g)

	return nil
}

func (ds *perDeviceState) collectMemoryUtilization() error {
	metricName := metricNameGPUUtilizationMemoryPercent
	g := pmetric.NewGauge()

	maxTimestamp := ds.getLastTimestamp(metricName)

	sampleType, samples, ret := ds.d.GetSamples(nvml.MEMORY_UTILIZATION_SAMPLES, maxTimestamp)
	if !errors.Is(ret, nvml.SUCCESS) {
		if errors.Is(ret, nvml.ERROR_NOT_FOUND) {
			slog.Warn("get MEMORY_UTILIZATION_SAMPLES not found", "err", ret)
			return nil
		}
		return fmt.Errorf("get MEMORY_UTILIZATION_SAMPLES failed %w", ret)
	}
	getValue, err := valueGetter(sampleType)
	if err != nil {
		return err
	}

	sort.Slice(samples, func(i, j int) bool {
		return samples[i].TimeStamp < samples[j].TimeStamp
	})

	for _, s := range samples {
		value := getValue(s.SampleValue).(int64)

		if s.TimeStamp == 0 {
			continue
		}
		if value < 0 || value > 100 { // ignore if below 0% or above 100%
			continue
		}

		maxTimestamp = max(maxTimestamp, s.TimeStamp)
		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	ds.appendGauge(metricName, maxTimestamp, g)

	return nil
}

func (ds *perDeviceState) collectProcessUtilization() error {
	util := pmetric.NewGauge()
	utilMem := pmetric.NewGauge()

	ts := time.Now()
	// Collect compute processes
	computeProcesses, ret := ds.d.GetComputeRunningProcesses()
	if !errors.Is(ret, nvml.SUCCESS) {
		return fmt.Errorf("failed to get compute running processes for %d: %s", ds.index, nvml.ErrorString(ret))
	}

	graphicsProccesses, ret := ds.d.GetGraphicsRunningProcesses()
	if !errors.Is(ret, nvml.SUCCESS) {
		return fmt.Errorf("failed to get graphics running processes for %d: %s", ds.index, nvml.ErrorString(ret))
	}

	// Return early if no processes are running
	if len(computeProcesses) == 0 && len(graphicsProccesses) == 0 {
		slog.Debug("no processes running")
		return nil
	}

	processes := append(computeProcesses, graphicsProccesses...)

	pids := make([]uint32, len(processes))
	for i, p := range processes {
		pids[i] = p.Pid
	}
	slog.Debug("processes running", "pids", pids)

	// Add data points for each process
	for _, process := range processes {
		utilization, ret := ds.d.GetProcessUtilization(uint64(process.Pid))
		if !errors.Is(ret, nvml.SUCCESS) {
			// If the process is not found (likely terminated), skip it and continue
			if errors.Is(ret, nvml.ERROR_NOT_FOUND) || errors.Is(ret, nvml.ERROR_NO_DATA) {
				slog.Debug("process not found, skipping", "pid", process.Pid, "error", nvml.ErrorString(ret))
				continue
			}
			return fmt.Errorf("failed to get process utilization for %d - pid: %d - %s", ds.index, process.Pid, nvml.ErrorString(ret))
		}

		processName, ret := nvml.SystemGetProcessName(int(process.Pid)) // could easily be cached
		if !errors.Is(ret, nvml.SUCCESS) {
			// If the process is not found (likely terminated), skip it and continue
			if errors.Is(ret, nvml.ERROR_NOT_FOUND) || errors.Is(ret, nvml.ERROR_NO_DATA) {
				slog.Debug("process not found, skipping", "pid", process.Pid, "error", nvml.ErrorString(ret))
				continue
			}
			return fmt.Errorf("failed to get process name for %d - pid: %d - %s", ds.index, process.Pid, nvml.ErrorString(ret))
		}

		for _, sample := range utilization {
			// Add utilization data point
			dpUtil := util.DataPoints().AppendEmpty()
			dpUtil.Attributes().PutStr(attributeUUID, ds.uuid)
			dpUtil.Attributes().PutInt(attributeIndex, int64(ds.index))
			dpUtil.Attributes().PutInt(attributePID, int64(process.Pid))
			dpUtil.Attributes().PutStr(attributeComm, processName)
			dpUtil.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
			dpUtil.SetIntValue(int64(sample.SmUtil))

			// Add memory utilization data point
			dpMem := utilMem.DataPoints().AppendEmpty()
			dpMem.Attributes().PutStr(attributeUUID, ds.uuid)
			dpMem.Attributes().PutInt(attributeIndex, int64(ds.index))
			dpMem.Attributes().PutInt(attributePID, int64(process.Pid))
			dpMem.Attributes().PutStr(attributeComm, processName)
			dpMem.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
			dpMem.SetIntValue(int64(sample.MemUtil))
		}
	}

	// Append gauges
	ds.appendGaugeWithoutTime(metricNameGPUUtilizationPercent, util)
	ds.appendGaugeWithoutTime(metricNameGPUUtilizationMemoryPercent, utilMem)

	return nil
}

func (ds *perDeviceState) collectClock() error {
	clockTypes := map[string]nvml.ClockType{
		"graphics": nvml.CLOCK_GRAPHICS,
		"sm":       nvml.CLOCK_SM,
		"mem":      nvml.CLOCK_MEM,
		"video":    nvml.CLOCK_VIDEO,
	}

	g := pmetric.NewGauge()

	for clockName, clockType := range clockTypes {
		ts := time.Now()
		clock, ret := nvml.DeviceGetClockInfo(ds.d, clockType)
		if !errors.Is(ret, nvml.SUCCESS) {
			// Allow NOT_SUPPORTED for DGX
			if errors.Is(ret, nvml.ERROR_NOT_SUPPORTED) {
				slog.Warn("clock not found", "device", ds.index, "clock", clockName, "err", ret)
			} else {
				return fmt.Errorf("failed to get clock for %d %s: %s", ds.index, clockName, nvml.ErrorString(ret))
			}
		}
		clock *= 1e6 // MHz to Hertz

		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.index))
		dp.Attributes().PutStr(attributeClock, clockName)
		dp.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
		dp.SetIntValue(int64(clock))
	}

	ds.appendGauge(metricNameGPUClockHertz, uint64(time.Now().UnixNano()), g)

	return nil
}

func (ds *perDeviceState) collectPowerConsumption() error {
	metricName := metricNameGPUPowerWatt
	g := pmetric.NewGauge()

	maxTimestamp := ds.getLastTimestamp(metricName)

	sampleType, samples, ret := ds.d.GetSamples(nvml.TOTAL_POWER_SAMPLES, maxTimestamp)
	if !errors.Is(ret, nvml.SUCCESS) {
		if errors.Is(ret, nvml.ERROR_NOT_FOUND) {
			slog.Warn("get TOTAL_POWER_SAMPLES returned not found", "err", ret)
			return nil
		}
		return fmt.Errorf("GetSamples failed %v", ret)
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
		value := getValue(s.SampleValue).(int64) / 1000 // divide from milli watts to watts
		if value > 10*1000 {                            // ignore if above 10k watt
			continue
		}
		if value < 0 { // ignore negative power consumption
			continue
		}

		maxTimestamp = max(maxTimestamp, s.TimeStamp)

		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.index))
		dp.SetTimestamp(pcommon.Timestamp(s.TimeStamp * 1000)) // micros to nanos
		dp.SetIntValue(value)
	}

	ds.appendGauge(metricName, maxTimestamp, g)

	return nil
}

func (ds *perDeviceState) collectTemperature() error {
	metricName := metricNameGPUTemperatureCelsius

	ts := time.Now()
	temp, ret := ds.d.GetTemperature(nvml.TEMPERATURE_GPU)
	if !errors.Is(ret, nvml.SUCCESS) {
		return fmt.Errorf("failed to get temperature for %d: %s", ds.index, nvml.ErrorString(ret))
	}

	g := pmetric.NewGauge()
	dp := g.DataPoints().AppendEmpty()
	dp.Attributes().PutStr(attributeUUID, ds.uuid)
	dp.Attributes().PutInt(attributeIndex, int64(ds.index))
	dp.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
	dp.SetIntValue(int64(temp))

	ds.appendGauge(metricName, uint64(ts.UnixNano()), g)

	return nil
}

var pcieCounters = []nvml.PcieUtilCounter{
	nvml.PCIE_UTIL_TX_BYTES,
	nvml.PCIE_UTIL_RX_BYTES,
	//nvml.PCIE_UTIL_COUNT, // not used until needed
}

func (ds *perDeviceState) collectPCIThroughput() error {
	for _, counter := range pcieCounters {
		ts := time.Now()

		tp, ret := ds.d.GetPcieThroughput(counter)
		if !errors.Is(ret, nvml.SUCCESS) {
			if errors.Is(ret, nvml.ERROR_NOT_SUPPORTED) {
				slog.Warn("failed to get PCIe throughput", "device", ds.index, "counter", counter, "err", ret)
				return nil
			} else {
				return fmt.Errorf("failed to get PCIe throughput for %d %d: %s", ds.index, counter, nvml.ErrorString(ret))
			}
		}

		var metricName string
		switch counter {
		case nvml.PCIE_UTIL_TX_BYTES:
			metricName = metricNameGPUPCIeThroughputTransmit
			tp *= 1000 // KB/s to bytes/s
		case nvml.PCIE_UTIL_RX_BYTES:
			metricName = metricNameGPUPCIeThroughputReceive
			tp *= 1000 // KB/s to bytes/s
		case nvml.PCIE_UTIL_COUNT:
			metricName = metricNameGPUPCIeThroughputCount
		}

		g := pmetric.NewGauge()
		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.index))
		dp.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
		dp.SetIntValue(int64(tp))

		ds.appendGauge(metricName, uint64(ts.UnixNano()), g)
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
