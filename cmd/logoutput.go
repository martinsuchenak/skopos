package cmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/logship"
	"github.com/paularlott/cli"
)

// Log output (the global --log-output-* flags): besides stdout, logs go to an
// HTTP log intake — VictoriaLogs, Loki, Elasticsearch/OpenSearch, Vector, a
// knot space's agent — the way knot ships its own (internal/logship).

// logOutputSettings are the flag values, resolved by the caller (the worker
// also reads its env file, so it passes resolveWorkerSetting).
type logOutputSettings struct{ URL, Format, Stream, Header string }

func logOutputFrom(cmd *cli.Command, resolve func(flagVal, envName, flagDefault string) string) logOutputSettings {
	if resolve == nil {
		resolve = func(v, _, _ string) string { return v }
	}
	return logOutputSettings{
		URL:    resolve(cmd.GetString("log-output-url"), "LOG_OUTPUT_URL", ""),
		Format: resolve(cmd.GetString("log-output-format"), "LOG_OUTPUT_FORMAT", "ndjson"),
		Stream: resolve(cmd.GetString("log-output-stream"), "LOG_OUTPUT_STREAM", "skopos"),
		Header: resolve(cmd.GetString("log-output-header"), "LOG_OUTPUT_HEADER", ""),
	}
}

// openLogOutput starts the sink; nil when no URL is set. Failures are
// reported on stderr at most once a minute — never through the logger
// itself, which would feed the failing sink.
func openLogOutput(s logOutputSettings) (*logship.Writer, error) {
	if s.URL == "" {
		return nil, nil
	}
	format, err := logship.ParseFormat(s.Format)
	if err != nil {
		return nil, err
	}
	headers, err := logship.ParseHeaders(s.Header)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var last time.Time
	return logship.New(logship.Config{
		URL: s.URL, Format: format, Stream: s.Stream, Headers: headers,
		OnError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(last) < time.Minute {
				return
			}
			last = time.Now()
			fmt.Fprintf(os.Stderr, "log output %s: %v (lines are kept and retried; the oldest go past 10000)\n", redactedURL(s.URL), err)
		},
	})
}

// logWriter is where the process logs: stdout, plus the sink when one is
// configured (then in JSON, the records the sink takes).
func logWriter(ship *logship.Writer, format string) (io.Writer, string) {
	if ship == nil {
		return os.Stdout, format
	}
	return io.MultiWriter(os.Stdout, ship), "json"
}

// closeLogOutput sends what is left (bounded) on exit.
func closeLogOutput(ship *logship.Writer) {
	if ship == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ship.Close(ctx)
}

// redactedURL hides a password in the URL (basic auth in the URL is common
// for an intake behind a proxy).
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable url)"
	}
	return u.Redacted()
}

// secrets are the credentials the settings carry — header values (and the
// token after an auth scheme), the URL's password — for the worker's run-log
// redactor.
func (s logOutputSettings) secrets() []string {
	var out []string
	if headers, err := logship.ParseHeaders(s.Header); err == nil {
		for _, v := range headers {
			out = append(out, v)
			if f := strings.Fields(v); len(f) > 1 {
				out = append(out, f[len(f)-1])
			}
		}
	}
	if u, err := url.Parse(s.URL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			out = append(out, pw, u.User.String())
		}
	}
	return out
}
