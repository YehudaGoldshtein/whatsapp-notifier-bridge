// Axiom sink for the slog logger. Buffers records in memory, flushes in a
// background goroutine every few seconds or when the buffer hits a size cap.
// Exits cleanly — the process shutdown path calls Close() which drains.
//
// Failure is always silent: logging is never allowed to crash the app, so a
// flaky Axiom endpoint just means dropped batches with a printf to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

type axiomHandler struct {
	inner     slog.Handler // tees stdout JSON as before
	endpoint  string
	token     string
	service   string
	flushEvery time.Duration
	maxBatch  int

	mu   sync.Mutex
	buf  []map[string]any
	stop chan struct{}
	wg   sync.WaitGroup
}

func newAxiomHandler(
	inner slog.Handler,
	apiURL, apiToken, dataset, service string,
) *axiomHandler {
	h := &axiomHandler{
		inner:      inner,
		endpoint:   fmt.Sprintf("%s/v1/datasets/%s/ingest", apiURL, dataset),
		token:      apiToken,
		service:    service,
		flushEvery: 3 * time.Second,
		maxBatch:   100,
		stop:       make(chan struct{}),
	}
	h.wg.Add(1)
	go h.drain()
	return h
}

func (h *axiomHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *axiomHandler) Handle(ctx context.Context, r slog.Record) error {
	// Always write to the inner handler first (stdout); Axiom is additive.
	innerErr := h.inner.Handle(ctx, r)

	entry := map[string]any{
		"_time":   r.Time.UTC().Format(time.RFC3339Nano),
		"level":   r.Level.String(),
		"msg":     r.Message,
		"service": h.service,
	}
	r.Attrs(func(a slog.Attr) bool {
		entry[a.Key] = a.Value.Any()
		return true
	})

	h.mu.Lock()
	h.buf = append(h.buf, entry)
	full := len(h.buf) >= h.maxBatch
	h.mu.Unlock()

	if full {
		go h.flush() // non-blocking; the drain loop covers the normal case
	}
	return innerErr
}

func (h *axiomHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &axiomHandler{
		inner:      h.inner.WithAttrs(attrs),
		endpoint:   h.endpoint,
		token:      h.token,
		service:    h.service,
		flushEvery: h.flushEvery,
		maxBatch:   h.maxBatch,
		buf:        h.buf,
		mu:         sync.Mutex{},
		stop:       h.stop,
	}
}

func (h *axiomHandler) WithGroup(name string) slog.Handler {
	return &axiomHandler{
		inner:      h.inner.WithGroup(name),
		endpoint:   h.endpoint,
		token:      h.token,
		service:    h.service,
		flushEvery: h.flushEvery,
		maxBatch:   h.maxBatch,
		buf:        h.buf,
		mu:         sync.Mutex{},
		stop:       h.stop,
	}
}

func (h *axiomHandler) drain() {
	defer h.wg.Done()
	t := time.NewTicker(h.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			h.flush()
			return
		case <-t.C:
			h.flush()
		}
	}
}

func (h *axiomHandler) flush() {
	h.mu.Lock()
	if len(h.buf) == 0 {
		h.mu.Unlock()
		return
	}
	batch := h.buf
	h.buf = nil
	h.mu.Unlock()

	body, err := json.Marshal(batch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[axiom] marshal failed: %v\n", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[axiom] new request failed: %v\n", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")

	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[axiom] POST failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Fprintf(os.Stderr, "[axiom] POST %d: %s\n", resp.StatusCode, string(b))
	}
}

// Close drains any buffered events and stops the background goroutine.
func (h *axiomHandler) Close() {
	close(h.stop)
	h.wg.Wait()
}
