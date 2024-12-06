package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	arrowpb "github.com/open-telemetry/otel-arrow/api/experimental/arrow/v1"
	// parcaflags "github.com/parca-dev/parca-agent/flags"
	"github.com/polarsignals/gpu-metrics-agent/flags"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/trace/noop"
)

func main() {
	os.Exit(int(mainWithExitCode()))
}

type buildInfo struct {
	GoArch, GoOs, VcsRevision, VcsTime string
	VcsModified                        bool
}

var (
	version string
	commit  string
	date    string
	goArch  string
)

func fetchBuildInfo() (*buildInfo, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, errors.New("can't read the build info")
	}

	buildInfo := buildInfo{}

	for _, setting := range bi.Settings {
		key := setting.Key
		value := setting.Value

		switch key {
		case "GOARCH":
			buildInfo.GoArch = value
		case "GOOS":
			buildInfo.GoOs = value
		case "vcs.revision":
			buildInfo.VcsRevision = value
		case "vcs.time":
			buildInfo.VcsTime = value
		case "vcs.modified":
			buildInfo.VcsModified = value == "true"
		}
	}

	return &buildInfo, nil
}

func mainWithExitCode() flags.ExitCode {
	ctx := context.Background()

	// Fetch build info such as the git revision we are based off
	buildInfo, err := fetchBuildInfo()
	if err != nil {
		fmt.Println("failed to fetch build info: %w", err) //nolint:forbidigo
		return flags.ExitFailure
	}
	if commit == "" {
		commit = buildInfo.VcsRevision
	}
	if date == "" {
		date = buildInfo.VcsTime
	}
	if goArch == "" {
		goArch = buildInfo.GoArch
	}

	f, err := flags.Parse()
	if err != nil {
		log.Errorf("Failed to parse flags: %v", err)
		return flags.ExitParseError
	}

	if f.Version {
		fmt.Printf("parca-agent, version %s (commit: %s, date: %s), arch: %s\n", version, commit, date, goArch) //nolint:forbidigo
		return flags.ExitSuccess
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewBuildInfoCollector(),
		collectors.NewGoCollector(
			collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll),
		),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	grpcConn, err := f.RemoteStore.WaitGrpcEndpoint(ctx, reg, noop.NewTracerProvider())
	if err != nil {
		log.Errorf("failed to connect to server: %v", err)
		return flags.ExitFailure
	}
	defer grpcConn.Close()


	arrowClient := arrowpb.NewArrowMetricsServiceClient(grpcConn)
	arrowMetricsExporter := NewExporter(arrowClient, time.Second*10, map[string]any{"foo": "bar"})
	const nvidiaMetricsScopeName = "parca.nvidia_gpu_metrics"
	if f.MetricsProducer.NvidiaGpu {
		nvidia, err := NewNvidiaProducer()
		if err != nil {
			return flags.Failure("Failed to instantiate nvidia metrics producer: %v. Are the Nvidia drivers installed?", err)
		}
		arrowMetricsExporter.AddProducer(ProducerConfig{
			Producer:  nvidia,
			ScopeName: nvidiaMetricsScopeName,
		})
	}
	if f.MetricsProducer.NvidiaGpuMock {
		mock := NewNvidiaMockProducer(3, time.Now())
		scopeName := nvidiaMetricsScopeName
		if f.MetricsProducer.NvidiaGpu {
			// don't conflict with the real producer
			scopeName = scopeName + "_mock"
		}
		arrowMetricsExporter.AddProducer(ProducerConfig{
			Producer:  mock,
			ScopeName: scopeName,
		})

	}
	arrowMetricsExporter.Start(ctx)

	// Block forever
	<-ctx.Done()
	
	return flags.ExitSuccess
}
