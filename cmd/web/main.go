// Command web serves the expense app UI and API. It starts, signals, and reads
// Workflows but runs none of them, so it stays up while workers come and go.
package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/temporal-sa/temporal-demo-expense-approval/internal/config"
	"github.com/temporal-sa/temporal-demo-expense-approval/web"
)

func main() {
	logger := config.Logger()
	opts, err := config.ClientOptions(logger)
	if err != nil {
		logger.Error("invalid Temporal client configuration", "error", err)
		os.Exit(1)
	}
	// Lazy: the UI comes up and reports Temporal as unavailable instead of
	// crashing when Temporal isn't reachable yet.
	c, err := client.NewLazyClient(opts)
	if err != nil {
		logger.Error("invalid Temporal client", "error", err)
		os.Exit(1)
	}
	defer c.Close()

	escalateAfter := duration(config.Env("ESCALATE_AFTER", "45s"))
	expireAfter := duration(config.Env("EXPIRE_AFTER", "10m"))
	if expireAfter <= escalateAfter {
		logger.Error("EXPIRE_AFTER must be longer than ESCALATE_AFTER")
		os.Exit(1)
	}

	srv := web.NewServer(web.Config{
		Client:        c,
		TaskQueue:     config.TaskQueue(),
		TemporalUIURL: temporalUIURL(opts),
		EscalateAfter: escalateAfter,
		ExpireAfter:   expireAfter,
		Logger:        logger,
	})

	addr := ":" + config.Env("PORT", "8080")
	logger.Info("web app listening", "addr", addr, "address", opts.HostPort, "namespace", opts.Namespace, "taskQueue", config.TaskQueue())
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		logger.Error("web app stopped", "error", err)
		os.Exit(1)
	}
}

// temporalUIURL is the namespace's page in the Temporal UI. TEMPORAL_UI_URL
// wins; otherwise a TLS or API-key connection is taken to be Temporal Cloud,
// and anything else the local dev server.
func temporalUIURL(opts client.Options) string {
	if v := config.Env("TEMPORAL_UI_URL", ""); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if opts.Credentials != nil || opts.ConnectionOptions.TLS != nil {
		return "https://cloud.temporal.io/namespaces/" + opts.Namespace
	}
	return "http://localhost:8233/namespaces/" + opts.Namespace
}

func duration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		config.Logger().Error("invalid duration", "value", s)
		os.Exit(1)
	}
	return d
}
