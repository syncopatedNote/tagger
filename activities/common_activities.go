package activities

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"dagger.io/dagger"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"
)

// connectDagger opens a connection to the Dagger engine, streaming the engine's
// log output to the worker's stderr. It is the single chokepoint every activity
// uses to reach Dagger, so engine-level observability (log level, output sink)
// is configured in exactly one place. The returned client MUST be closed by the
// caller (defer client.Close()).
//
// The activity's ctx is passed straight through: when Temporal cancels the
// activity (StartToCloseTimeout or HeartbeatTimeout fires), ctx is cancelled and
// every in-flight Dagger operation aborts — this is how a Temporal-level cap
// actually stops a runaway pipeline.
func connectDagger(ctx context.Context) (*dagger.Client, error) {
	// Verbosity controls how much the engine's progress stream prints to stderr.
	// 0 (the default) = span names only; 3 (-vvv) surfaces span debug logs, which
	// include the LLM message turns and tool-call request/response bodies. Tuned
	// via DAGGER_VERBOSITY in .env so it can be dialled up for debugging without a
	// code change. Pair with OTEL_EXPORTER_OTLP_TRACES_LIVE=1 for live (un-batched)
	// streaming so a long LLM loop shows progress instead of looking frozen.
	verbosity := getenvInt("DAGGER_VERBOSITY", 0)
	client, err := dagger.Connect(ctx,
		dagger.WithLogOutput(os.Stderr),
		dagger.WithVerbosity(verbosity),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to dagger engine: %w", err)
	}
	return client, nil
}

// startHeartbeat records a Temporal heartbeat on an interval until the returned
// stop function is called. Heartbeating lets the HeartbeatTimeout detect a stuck
// worker long before the (much larger) StartToCloseTimeout would.
func startHeartbeat(ctx context.Context, every time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx, "dagger agent running")
			}
		}
	}()
	return func() { close(done) }
}

// startHeartbeatWithProgress is startHeartbeat plus a proof-of-life log line on
// every tick. The Dagger LLM loop runs as ONE blocking call and emits its turns
// as OTel spans the worker doesn't print, so without this the worker looks dead
// during a long crawl. Each tick logs elapsed time so you can see the activity is
// alive and roughly how long the model has been working.
func startHeartbeatWithProgress(ctx context.Context, every time.Duration, logger log.Logger) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				elapsed := time.Since(start).Round(time.Second)
				activity.RecordHeartbeat(ctx, "dagger agent running")
				logger.Info("Context agent still running (Dagger LLM loop in progress)", "elapsed", elapsed.String())
			}
		}
	}()
	return func() { close(done) }
}

// getenv returns the value of the environment variable key, or def if it is
// unset or empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getenvInt returns the integer value of the environment variable key, or def if
// it is unset, empty, or not a valid integer.
func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// truncate returns at most the first n characters of s, appending "..." when s
// was longer. Used to keep large model output / errors from flooding the logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
