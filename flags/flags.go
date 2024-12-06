package flags

import (
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kong"
	parcaflags "github.com/parca-dev/parca-agent/flags"
	log "github.com/sirupsen/logrus"
)

type ExitCode int

const (
	ExitSuccess ExitCode = 0
	ExitFailure ExitCode = 1

	// Go 'flag' package calls os.Exit(2) on flag parse errors, if ExitOnError is set
	ExitParseError ExitCode = 2
)

func Failure(msg string, args ...interface{}) ExitCode {
	log.Errorf(msg, args...)
	return ExitFailure
}

type Flags struct {
	Version           bool          `help:"Show application version."`
	Log               FlagsLogs     `embed:""                         prefix:"log-"`
	Node              string        `default:"${hostname}"               help:"The name of the node that the process is running on. If on Kubernetes, this must match the Kubernetes node name."`
	ClockSyncInterval time.Duration `default:"3m" help:"How frequently to synchronize with the realtime clock."`

	// which metrics producers (e.g. nvidia) to enable
	MetricsProducer FlagsMetricProducer `embed:"" prefix:"metrics-producer-"`
	RemoteStore     parcaflags.FlagsRemoteStore    `embed:"" prefix:"remote-store-"`
}

// FlagsLocalStore provides logging configuration flags.
type FlagsLogs struct {
	Level  string `default:"info"   enum:"error,warn,info,debug" help:"Log level."`
	Format string `default:"logfmt" enum:"logfmt,json"           help:"Configure if structured logging as JSON or as logfmt"`
}

func (f FlagsLogs) logrusLevel() log.Level {
	switch f.Level {
	case "error":
		return log.ErrorLevel
	case "warn":
		return log.WarnLevel
	case "info":
		return log.InfoLevel
	case "debug":
		return log.DebugLevel
	default:
		return log.InfoLevel
	}
}

func (f FlagsLogs) logrusFormatter() log.Formatter {
	switch f.Format {
	case "logfmt":
		return &log.TextFormatter{}
	case "json":
		return &log.JSONFormatter{}
	default:
		return &log.TextFormatter{}
	}
}

func (f FlagsLogs) ConfigureLogger() {
	log.SetLevel(f.logrusLevel())
	log.SetFormatter(f.logrusFormatter())
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
