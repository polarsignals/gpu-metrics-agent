# GPU Metrics Agent

A high-performance agent for collecting NVIDIA GPU metrics and exporting them via OpenTelemetry Arrow protocol. Developed by [Polar Signals](https://www.polarsignals.com) for comprehensive GPU observability.

## Why GPU Metrics Agent?

Modern GPU workloads require deep visibility into resource utilization, performance characteristics, and system behavior. This agent provides the metrics you need to optimize GPU usage, reduce costs, and troubleshoot performance issues.

## Use Cases

### 🎯 GPU Resource Optimization
Track GPU and memory utilization to identify underutilized resources. Optimize batch sizes and workload scheduling based on actual GPU usage patterns.

**Key metrics**: `gpu_utilization_percent`, `gpu_utilization_memory_percent`

### 👥 Multi-tenant GPU Sharing
Monitor per-process GPU utilization to ensure fair resource allocation in shared environments. Track which processes are consuming GPU resources and enforce usage policies.

**Key metrics**: Per-process `gpu_utilization_percent` with `pid` and `comm` attributes

### 🔍 Performance Troubleshooting
Identify performance bottlenecks by correlating GPU metrics with application behavior. Detect thermal throttling, power limitations, and PCIe bandwidth constraints.

**Key metrics**: `gpu_temperature_celsius`, `gpu_clock_hertz`, `gpu_pcie_throughput_*_bytes`

### 💰 Cost Management
Monitor power consumption to calculate operational costs and optimize for efficiency. Track power usage trends and identify opportunities for cost reduction.

**Key metrics**: `gpu_power_watt`, `gpu_power_limit_watt`

### 📊 Capacity Planning
Analyze historical utilization patterns to plan for future GPU infrastructure needs. Understand peak usage times and resource requirements for different workloads.

**Key metrics**: All utilization metrics with time-series analysis

### 🤖 ML/AI Workload Monitoring
Track GPU performance during model training and inference. Ensure optimal resource allocation for different phases of machine learning pipelines.

**Key metrics**: All metrics combined with workload-specific context

## Collected Metrics

| Metric | Description | Collection Interval |
|--------|-------------|-------------------|
| `gpu_utilization_percent` | GPU compute utilization (0-100%) | 5s |
| `gpu_utilization_memory_percent` | GPU memory utilization (0-100%) | 5s |
| `gpu_power_watt` | Current power consumption | 1s |
| `gpu_power_limit_watt` | Maximum power limit | 1s |
| `gpu_clock_hertz` | Clock speeds (graphics, SM, memory, video) | 1s |
| `gpu_temperature_celsius` | GPU temperature | 1s |
| `gpu_pcie_throughput_transmit_bytes` | PCIe transmit throughput | 100ms |
| `gpu_pcie_throughput_receive_bytes` | PCIe receive throughput | 100ms |
| `gpu_prof_dram_active` | Fraction of time the GPU memory interface is active (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_sm_active` | Fraction of time ≥1 warp is active on an SM (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_sm_occupancy` | Fraction of SM warp slots occupied (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_pipe_tensor_active` | Tensor-core pipe issue fraction (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_pipe_fp64_active` | FP64 pipe issue fraction (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_pipe_fp32_active` | FP32 pipe issue fraction (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |
| `gpu_prof_pipe_fp16_active` | FP16 pipe issue fraction (0..1). Requires `--metrics-producer-dcgm-profiling`. | 1s |

All metrics include `uuid` (GPU identifier) and `index` (GPU index) attributes. Process-level metrics also include `pid` and `comm` attributes.

## Quick Start

### Installation

For Kubernetes deployments, see our [comprehensive setup guide](https://www.polarsignals.com/docs/setup-collection-kubernetes-gpu).

### Basic Usage

```bash
./gpu-metrics-agent \
  --remote-store-address=grpc.polarsignals.com:443 \
  --metrics-producer-nvidia-gpu=true \
  --node=$(hostname) \
  --log-level=info
```

### Docker

```bash
docker run -it --rm \
  --gpus all \
  -v /etc/machine-id:/etc/machine-id:ro \
  -v /var/run/secrets/polarsignals.com:/var/run/secrets/polarsignals.com:ro \
  ghcr.io/polarsignals/gpu-metrics-agent:latest \
  --remote-store-address=grpc.polarsignals.com:443 \
  --metrics-producer-nvidia-gpu=true
```

### Configuration Options

| Flag | Description | Default |
|------|-------------|---------|
| `--remote-store-address` | gRPC endpoint for metric storage | Required |
| `--metrics-producer-nvidia-gpu` | Enable NVIDIA GPU metrics | false |
| `--metrics-producer-dcgm-profiling` | Enable DCGM profiling metrics (DRAM bandwidth, SM/pipe activity). See [DCGM profiling](#dcgm-profiling-optional) below. | false |
| `--collection-interval` | Metric export interval | 10s |
| `--node` | Node name for metric labeling | Machine ID |
| `--bearer-token` | Authentication token | - |

## Architecture

The agent consists of three main components:

1. **NVIDIA Producer**: Interfaces with NVIDIA Management Library (NVML) to collect GPU metrics
2. **Metric Exporter**: Batches and exports metrics using OpenTelemetry Arrow protocol
3. **gRPC Client**: Manages secure connections to remote storage endpoints

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  NVIDIA GPUs    │────▶│  GPU Metrics     │────▶│ Remote Storage  │
│  (via NVML)     │     │  Agent           │     │ (gRPC/OTel)    │
└─────────────────┘     └──────────────────┘     └─────────────────┘
```

## Requirements

- NVIDIA GPU with driver version 390.x or newer
- Linux operating system
- NVIDIA Management Library (NVML) available

### DCGM profiling (optional)

Setting `--metrics-producer-dcgm-profiling=true` loads `libdcgm.so` in-process (DCGM *embedded* mode — no separate `nv-hostengine` daemon). While enabled, the agent acquires the GPU's PerfWorks counter subsystem **exclusively**: NVIDIA Nsight Compute (`ncu`) and any CUPTI Profiling API clients on the same GPU will fail to start. The CUPTI callback API used for kernel-launch tracing (Parca GPU profiler / `libparcagpucupti.so`) is unaffected.

Multiply the `gpu_prof_dram_active` fraction by the GPU's peak memory bandwidth (e.g. L4 = 300 GB/s, A100 = 1.55 TB/s, H100 = 3.35 TB/s) to get sustained bandwidth in bytes/sec.

`libdcgm.so` is not bundled in the container image — like `libnvidia-ml.so`, it must be supplied at runtime. The simplest options:

- **NVIDIA Container Toolkit / CDI**: configure the CDI spec to inject `libdcgm.so.*` from the host (the host needs `datacenter-gpu-manager` installed, e.g. `apt install datacenter-gpu-manager` on Debian/Ubuntu).
- **Bind mount**: `-v /usr/lib/x86_64-linux-gnu/libdcgm.so.3:/usr/lib/x86_64-linux-gnu/libdcgm.so.3` (or the equivalent aarch64 path).
- **Build from source**: install `libdcgm` on the target host directly.

If `libdcgm.so` cannot be loaded the agent will exit with `Failed to instantiate DCGM profiling producer: ... Is libdcgm.so present and the NVIDIA driver loaded?`.

## Building from Source

```bash
go build .
```

## License

Apache License 2.0

## Support

- Documentation: [polarsignals.com/docs](https://www.polarsignals.com/docs)
- Issues: [GitHub Issues](https://github.com/polarsignals/gpu-metrics-agent/issues)