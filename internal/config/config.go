// Package config holds the settings shared by the web app and the worker.
// Everything comes from the environment so the same image runs locally, on
// Temporal Cloud, and in a tmprl-demo.cloud crashable workspace.
package config

import (
	"log/slog"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
	"go.temporal.io/sdk/log"
)

const (
	// TaskQueueEnv names the one variable every worker and client reads for its
	// task queue. A crashable workspace overrides it with <base>-<workspace id>.
	TaskQueueEnv     = "TEMPORAL_TASK_QUEUE"
	DefaultTaskQueue = "expense-approval"
)

// TaskQueue returns the task queue this process starts work on or polls.
func TaskQueue() string {
	return Env(TaskQueueEnv, DefaultTaskQueue)
}

// ClientOptions reads TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, TEMPORAL_API_KEY,
// TEMPORAL_TLS*, and related variables. The CLI config file is ignored on
// purpose: a developer's default profile may point at a real Cloud namespace.
func ClientOptions(logger *slog.Logger) (client.Options, error) {
	opts, err := envconfig.LoadClientOptions(envconfig.LoadClientOptionsRequest{DisableFile: true})
	if err != nil {
		return client.Options{}, err
	}
	if opts.HostPort == "" {
		opts.HostPort = "localhost:7233"
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	opts.Logger = storyLogger{log.NewStructuredLogger(logger)}
	return opts, nil
}

// Logger returns a logfmt logger, which reads well in a terminal or in k9s.
func Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// Env returns the variable's value, or def when it is unset or blank.
func Env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// storyLogger rewords the SDK's per-attempt activity error. Every attempt in
// this demo is retried by the Retry Policy, and "ERROR Activity error." on a
// shared screen reads like an outage rather than Temporal doing its job.
type storyLogger struct{ log.Logger }

func (l storyLogger) Error(msg string, keyvals ...any) {
	if msg == "Activity error." {
		l.Logger.Warn("activity attempt failed; Temporal will retry it", keyvals...)
		return
	}
	l.Logger.Error(msg, keyvals...)
}

func (l storyLogger) With(keyvals ...any) log.Logger {
	if wl, ok := l.Logger.(log.WithLogger); ok {
		return storyLogger{wl.With(keyvals...)}
	}
	return l
}
