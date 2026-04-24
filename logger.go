// Package-level structured logger. Writes JSON to stdout today; a later change
// swaps the handler for a multi-handler that also forwards to Axiom without
// touching any call sites.
//
// Usage:
//	log.Info("sent_message", "caller", caller, "to", jid, "id", resp.ID)
//	log.Warn("qr_channel_reopen", "reason", "timeout")
//	log.Error("sqlstore_failed", "error", err)
//
// Keep event names snake_case — they become fields in Axiom / anywhere else
// we aggregate logs across services.
package main

import (
	"log/slog"
	"os"
)

var log *slog.Logger

func init() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	log = slog.New(handler).With("service", "whatsapp-notifier-bridge")
}
