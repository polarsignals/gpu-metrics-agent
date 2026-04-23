//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	metricNameGPUProfDRAMActive       = "gpu_prof_dram_active"
	metricNameGPUProfSMActive         = "gpu_prof_sm_active"
	metricNameGPUProfSMOccupancy      = "gpu_prof_sm_occupancy"
	metricNameGPUProfPipeTensorActive = "gpu_prof_pipe_tensor_active"
	metricNameGPUProfPipeFP64Active   = "gpu_prof_pipe_fp64_active"
	metricNameGPUProfPipeFP32Active   = "gpu_prof_pipe_fp32_active"
	metricNameGPUProfPipeFP16Active   = "gpu_prof_pipe_fp16_active"

	dcgmPollInterval      = 1 * time.Second
	dcgmUpdateFreqUs      = int64(100_000) // 100 ms backend sampling
	dcgmMaxKeepAgeSeconds = float64(3600)
	dcgmMaxKeepSamples    = int32(0) // 0 = keep all within maxKeepAge
	dcgmFieldGroupName    = "gpu-metrics-agent-prof"
)

// dcgmProfFields maps DCGM profiling field IDs to metric names.
// Values are fractions in [0, 1]; multiply by peak per-GPU spec to get absolute rates.
var dcgmProfFields = map[dcgm.Short]string{
	dcgm.DCGM_FI_PROF_DRAM_ACTIVE:        metricNameGPUProfDRAMActive,
	dcgm.DCGM_FI_PROF_SM_ACTIVE:          metricNameGPUProfSMActive,
	dcgm.DCGM_FI_PROF_SM_OCCUPANCY:       metricNameGPUProfSMOccupancy,
	dcgm.DCGM_FI_PROF_PIPE_TENSOR_ACTIVE: metricNameGPUProfPipeTensorActive,
	dcgm.DCGM_FI_PROF_PIPE_FP64_ACTIVE:   metricNameGPUProfPipeFP64Active,
	dcgm.DCGM_FI_PROF_PIPE_FP32_ACTIVE:   metricNameGPUProfPipeFP32Active,
	dcgm.DCGM_FI_PROF_PIPE_FP16_ACTIVE:   metricNameGPUProfPipeFP16Active,
}

type DcgmProducer struct {
	cleanup      func()
	fieldIDs     []dcgm.Short
	fieldGroupID dcgm.FieldHandle
	devices      []*dcgmPerDeviceState
}

type dcgmPerDeviceState struct {
	gpuID uint
	uuid  string

	mu          *sync.RWMutex
	gauges      map[string]pmetric.Gauge
	unsupported map[dcgm.Short]bool
}

func NewDcgmProducer() (_ *DcgmProducer, retErr error) {
	slog.Warn("DCGM profiling producer enabled: acquires GPU PerfWorks counters exclusively; ncu and CUPTI profiling API on the same GPU will fail while this is running")

	shutdown, err := dcgm.Init(dcgm.Embedded)
	if err != nil {
		return nil, fmt.Errorf("dcgm.Init(Embedded) failed: %w", err)
	}
	defer func() {
		if retErr != nil {
			shutdown()
		}
	}()

	gpuIDs, err := dcgm.GetSupportedDevices()
	if err != nil {
		return nil, fmt.Errorf("dcgm.GetSupportedDevices failed: %w", err)
	}
	if len(gpuIDs) == 0 {
		return nil, fmt.Errorf("no DCGM-supported GPUs found")
	}

	devices := make([]*dcgmPerDeviceState, 0, len(gpuIDs))
	for _, gpuID := range gpuIDs {
		info, err := dcgm.GetDeviceInfo(gpuID)
		if err != nil {
			return nil, fmt.Errorf("dcgm.GetDeviceInfo(%d) failed: %w", gpuID, err)
		}
		devices = append(devices, &dcgmPerDeviceState{
			gpuID:       gpuID,
			uuid:        info.UUID,
			mu:          &sync.RWMutex{},
			gauges:      map[string]pmetric.Gauge{},
			unsupported: map[dcgm.Short]bool{},
		})
	}

	fieldIDs := make([]dcgm.Short, 0, len(dcgmProfFields))
	for id := range dcgmProfFields {
		fieldIDs = append(fieldIDs, id)
	}

	fieldGroupID, err := dcgm.FieldGroupCreate(dcgmFieldGroupName, fieldIDs)
	if err != nil {
		return nil, fmt.Errorf("dcgm.FieldGroupCreate failed: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = dcgm.FieldGroupDestroy(fieldGroupID)
		}
	}()

	if err := dcgm.WatchFieldsWithGroupEx(
		fieldGroupID,
		dcgm.GroupAllGPUs(),
		dcgmUpdateFreqUs,
		dcgmMaxKeepAgeSeconds,
		dcgmMaxKeepSamples,
	); err != nil {
		return nil, fmt.Errorf("dcgm.WatchFieldsWithGroupEx failed: %w", err)
	}

	return &DcgmProducer{
		cleanup: func() {
			_ = dcgm.UnwatchFields(fieldGroupID, dcgm.GroupAllGPUs())
			_ = dcgm.FieldGroupDestroy(fieldGroupID)
			shutdown()
		},
		fieldIDs:     fieldIDs,
		fieldGroupID: fieldGroupID,
		devices:      devices,
	}, nil
}

func (p *DcgmProducer) Close() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

func (p *DcgmProducer) Collect(ctx context.Context) error {
	ticker := time.NewTicker(dcgmPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, pds := range p.devices {
				if err := pds.sample(p.fieldIDs); err != nil {
					return err
				}
			}
		}
	}
}

// isBlankFloat64 returns true if the value is one of DCGM's FP64 sentinel
// markers (BLANK, NOT_FOUND, NOT_SUPPORTED, NOT_PERMISSIONED), all of which
// are >= DCGM_FT_FP64_BLANK and well above any legitimate 0..1 fraction.
func isBlankFloat64(v float64) bool {
	return v >= dcgm.DCGM_FT_FP64_BLANK
}

func (ds *dcgmPerDeviceState) sample(fieldIDs []dcgm.Short) error {
	values, err := dcgm.GetLatestValuesForFields(ds.gpuID, fieldIDs)
	if err != nil {
		return fmt.Errorf("dcgm.GetLatestValuesForFields(gpu=%d) failed: %w", ds.gpuID, err)
	}

	ts := time.Now()
	for _, fv := range values {
		metricName, ok := dcgmProfFields[fv.FieldID]
		if !ok {
			continue
		}

		ds.mu.RLock()
		skipped := ds.unsupported[fv.FieldID]
		ds.mu.RUnlock()
		if skipped {
			continue
		}

		if fv.Status != 0 {
			slog.Warn("DCGM field unsupported on device; excluding from future samples",
				"gpu", ds.gpuID, "uuid", ds.uuid, "field", metricName, "status", fv.Status)
			ds.mu.Lock()
			ds.unsupported[fv.FieldID] = true
			ds.mu.Unlock()
			continue
		}

		value := fv.Float64()
		if isBlankFloat64(value) {
			slog.Warn("DCGM field returned blank value on device; excluding from future samples",
				"gpu", ds.gpuID, "uuid", ds.uuid, "field", metricName, "value", value)
			ds.mu.Lock()
			ds.unsupported[fv.FieldID] = true
			ds.mu.Unlock()
			continue
		}

		g := pmetric.NewGauge()
		dp := g.DataPoints().AppendEmpty()
		dp.Attributes().PutStr(attributeUUID, ds.uuid)
		dp.Attributes().PutInt(attributeIndex, int64(ds.gpuID))
		dp.SetTimestamp(pcommon.Timestamp(ts.UnixNano()))
		dp.SetDoubleValue(value)

		ds.appendGauge(metricName, g)
	}

	return nil
}

func (ds *dcgmPerDeviceState) appendGauge(metricName string, g pmetric.Gauge) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if existing, found := ds.gauges[metricName]; found {
		g.DataPoints().MoveAndAppendTo(existing.DataPoints())
	} else {
		ds.gauges[metricName] = g
	}
}

func (p *DcgmProducer) Produce(ms pmetric.MetricSlice) error {
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
	}
	return nil
}
