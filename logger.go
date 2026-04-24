// Package-level structured logger. Writes JSON to stdout always; when
// AXIOM_API_TOKEN is set, also forwards to Axiom via a batching handler.
package main

import (
	"log/slog"
	"os"
	"strings"
)

var (
	log        *slog.Logger
	logCleanup func() // called at process shutdown to drain async log sinks
)

func init() {
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})

	var handler slog.Handler = base
	token := strings.TrimSpace(os.Getenv("AXIOM_API_TOKEN"))
	dataset := strings.TrimSpace(os.Getenv("AXIOM_DATASET"))
	apiURL := strings.TrimSpace(os.Getenv("AXIOM_API_URL"))
	if apiURL == "" {
		apiURL = "https://api.axiom.co"
	}
	if token != "" && dataset != "" {
		ax := newAxiomHandler(base, apiURL, token, dataset, "whatsapp-notifier-bridge")
		handler = ax
		logCleanup = ax.Close
	}

	log = slog.New(handler).With("service", "whatsapp-notifier-bridge")
}
