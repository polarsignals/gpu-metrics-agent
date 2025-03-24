package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/alecthomas/kong"
)

type ExitCode int

const (
	ExitSuccess ExitCode = 0
	ExitFailure ExitCode = 1

	// Go 'flag' package calls os.Exit(2) on flag parse errors, if ExitOnError is set.
	ExitParseError ExitCode = 2
)

func Failure(msg string, args ...interface{}) ExitCode {
	slog.Error(msg, args...)
	return ExitFailure
}

type Flags struct {
	Version           bool          `help:"Show application version."`
	Log               FlagsLogs     `embed:""                         prefix:"log-"`
	Node              string        `default:"${hostname}"               help:"The name of the node that the process is running on. If on Kubernetes, this must match the Kubernetes node name."`
	ClockSyncInterval time.Duration `default:"3m" help:"How frequently to synchronize with the realtime clock."`

	// which metrics producers (e.g. nvidia) to enable
	MetricsProducer FlagsMetricProducer `embed:"" prefix:"metrics-producer-"`
	RemoteStore     FlagsRemoteStore    `embed:"" prefix:"remote-store-"`
}

// FlagsRemoteStore provides remote store configuration flags.
type FlagsRemoteStore struct {
	Address            string `help:"gRPC address to send profiles and symbols to."`
	BearerToken        string `kong:"help='Bearer token to authenticate with store.',env='PARCA_BEARER_TOKEN'"`
	BearerTokenFile    string `help:"File to read bearer token from to authenticate with store."`
	Insecure           bool   `help:"Send gRPC requests via plaintext instead of TLS."`
	InsecureSkipVerify bool   `help:"Skip TLS certificate verification."`

	BatchWriteInterval time.Duration `default:"10s"   help:"[deprecated] Interval between batch remote client writes. Leave this empty to use the default value of 10s."`
	RPCLoggingEnable   bool          `default:"false" help:"[deprecated] Enable gRPC logging."`
	RPCUnaryTimeout    time.Duration `default:"5m"    help:"[deprecated] Maximum timeout window for unary gRPC requests including retries."`

	GRPCMaxCallRecvMsgSize   int           `default:"33554432" help:"The maximum message size the client can receive."`
	GRPCMaxCallSendMsgSize   int           `default:"33554432" help:"The maximum message size the client can send."`
	GRPCStartupBackoffTime   time.Duration `default:"1m" help:"The time between failed gRPC requests during startup phase."`
	GRPCConnectionTimeout    time.Duration `default:"3s" help:"The timeout duration for gRPC connection establishment."`
	GRPCMaxConnectionRetries uint32        `default:"5" help:"The maximum number of retries to establish a gRPC connection."`
}

// FlagsLocalStore provides logging configuration flags.
type FlagsLogs struct {
	Level  string `default:"info"   enum:"error,warn,info,debug" help:"Log level."`
	Format string `default:"logfmt" enum:"logfmt,json"           help:"Configure if structured logging as JSON or as logfmt"`
}

// slogHandler returns a non-nil slog.Handler based on the log flags.
func (f FlagsLogs) slogHandler() slog.Handler {
	level := slog.LevelInfo
	switch f.Level {
	case "error":
		level = slog.LevelError
	case "warn":
		level = slog.LevelWarn
	case "info":
		level = slog.LevelInfo
	case "debug":
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}
	switch f.Format {
	case "logfmt":
		return slog.NewTextHandler(os.Stderr, opts)
	case "json":
		return slog.NewJSONHandler(os.Stderr, opts)
	default:
		return slog.NewTextHandler(os.Stderr, opts)
	}
}

func (f FlagsLogs) ConfigureLogger() {
	handler := f.slogHandler()
	if handler == nil {
		panic("slogHandler documented to return non-nil")
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

// FlagsMetricProducer contains flags that configure arrow metrics production.
type FlagsMetricProducer struct {
	NvidiaGpu     bool `default:"false" help:"Collect metrics related to Nvidia GPUs."`
	NvidiaGpuMock bool `default:"false" help:"Generate fake Nvidia GPU metrics." hidden:""`
}

func Parse() (Flags, error) {
	flags := Flags{}
	hostname, hostnameErr := os.Hostname() // hotnameErr handled below.
	kong.Parse(&flags, kong.Vars{
		"hostname": hostname,
	})

	if flags.Node == "" && hostnameErr != nil {
		return Flags{}, fmt.Errorf("failed to get hostname. Please set it with the --node flag: %w", hostnameErr)
	}
	flags.Log.ConfigureLogger()

	return flags, nil
}
