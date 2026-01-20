package sentry

import (
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
)

const dsn = "https://42839fd2e72bda2d97e6efcf6fc7d095@telebugs.lookhere.tech/api/v1/sentry_errors/1"

// Init initializes Sentry with hardcoded DSN and tracing disabled.
func Init() {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		EnableTracing:    false,
		TracesSampleRate: 0.0,
	})
	if err != nil {
		slog.Error("Failed to initialize Sentry", "error", err)
		return
	}
	slog.Info("Sentry initialized")
}

// Flush flushes any buffered events before shutdown.
func Flush() {
	sentry.Flush(2 * time.Second)
}

// CaptureError captures an error with optional context tags.
func CaptureError(err error, tags map[string]string) {
	if err == nil {
		return
	}

	sentry.WithScope(func(scope *sentry.Scope) {
		for k, v := range tags {
			scope.SetTag(k, v)
		}
		sentry.CaptureException(err)
	})
}

// CaptureWideEvent captures an error with full wide event context.
// This follows the wide events / canonical log lines pattern where all
// context accumulated during an operation is sent together.
func CaptureWideEvent(err error, event map[string]any) {
	if err == nil {
		return
	}

	sentry.WithScope(func(scope *sentry.Scope) {
		// Set high-cardinality fields as tags for filtering
		if opID, ok := event["operation_id"].(string); ok {
			scope.SetTag("operation_id", opID)
		}
		if opType, ok := event["operation_type"].(string); ok {
			scope.SetTag("operation_type", opType)
		}
		if outcome, ok := event["outcome"].(string); ok {
			scope.SetTag("outcome", outcome)
		}

		// Set registry context as tags
		if registry, ok := event["registry"].(map[string]any); ok {
			if name, ok := registry["name"].(string); ok {
				scope.SetTag("registry_name", name)
			}
			if addr, ok := registry["address"].(string); ok {
				scope.SetTag("registry_address", addr)
			}
		}

		// Set error phase as tag
		if errCtx, ok := event["error"].(map[string]any); ok {
			if phase, ok := errCtx["phase"].(string); ok {
				scope.SetTag("error_phase", phase)
			}
		}

		// Set the full wide event as extra context
		scope.SetContext("wide_event", event)

		sentry.CaptureException(err)
	})
}

// RecoverAndCapture recovers from a panic, sends it to Sentry, and re-panics.
// Use as: defer sentry.RecoverAndCapture(tags)
func RecoverAndCapture(tags map[string]string) {
	if r := recover(); r != nil {
		sentry.WithScope(func(scope *sentry.Scope) {
			for k, v := range tags {
				scope.SetTag(k, v)
			}
			sentry.CurrentHub().Recover(r)
			sentry.Flush(2 * time.Second)
		})
		panic(r)
	}
}
