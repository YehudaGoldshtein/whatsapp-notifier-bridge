// Minimal WhatsApp notifier bridge.
//
// Exposes POST /api/send for text-only notifications. Multi-tenant via
// auth tokens — a single bridge can serve many client projects, each with
// its own token. Sessions persist in SQLite under STORE_DIR (point this at
// a persistent volume in production).
//
// Env:
//   STORE_DIR        directory for whatsmeow.db (default: ./store)
//   PORT             HTTP port (default: 8080)
//   AUTH_TOKENS      comma-separated token:caller pairs, e.g.
//                    "tok_abc:inventory-sync,tok_def:dating-crm"
//                    If unset, API is unauthenticated (dev only).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	qrcode "github.com/skip2/go-qrcode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"google.golang.org/protobuf/proto"
	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// qrHolder publishes the latest pending QR code from whatsmeow so the
// /qr HTTP endpoint can render it on demand. Cleared once login succeeds.
type qrHolder struct {
	mu   sync.RWMutex
	code string
}

func (h *qrHolder) set(code string) { h.mu.Lock(); h.code = code; h.mu.Unlock() }
func (h *qrHolder) get() string     { h.mu.RLock(); defer h.mu.RUnlock(); return h.code }

type sendRequest struct {
	Recipient  string `json:"recipient"` // phone number in international format, e.g. 972504265054
	Message    string `json:"message"`
	CustomerID string `json:"customer_id,omitempty"` // optional tenant label; echoed to logs
}

type sendResponse struct {
	OK        bool   `json:"ok"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func loadAuthTokens() map[string]string {
	raw := strings.TrimSpace(os.Getenv("AUTH_TOKENS"))
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(parts) == 2 && parts[0] != "" {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func authMiddleware(tokens map[string]string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			next(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		caller, ok := tokens[token]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), "caller", caller))
		next(w, r)
	}
}

func recipientJID(recipient string) types.JID {
	r := strings.TrimPrefix(recipient, "+")
	if strings.Contains(r, "@") {
		if jid, err := types.ParseJID(r); err == nil {
			return jid
		}
	}
	return types.NewJID(r, types.DefaultUserServer)
}

func sendHandler(client *whatsmeow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, sendResponse{Error: "invalid json"})
			return
		}
		if req.Recipient == "" || req.Message == "" {
			writeJSON(w, http.StatusBadRequest, sendResponse{Error: "recipient and message required"})
			return
		}
		if !client.IsConnected() || client.Store.ID == nil {
			writeJSON(w, http.StatusServiceUnavailable, sendResponse{Error: "whatsapp not connected"})
			return
		}
		jid := recipientJID(req.Recipient)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		resp, err := client.SendMessage(ctx, jid, &waProto.Message{
			Conversation: proto.String(req.Message),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, sendResponse{Error: err.Error()})
			return
		}
		caller, _ := r.Context().Value("caller").(string)
		fields := []any{"caller", caller, "to", jid.String(), "id", resp.ID, "bytes", len(req.Message)}
		if req.CustomerID != "" {
			fields = append(fields, "customer_id", req.CustomerID)
		}
		log.Info("send_ok", fields...)
		writeJSON(w, http.StatusOK, sendResponse{OK: true, MessageID: resp.ID})
	}
}

func healthHandler(client *whatsmeow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Always 200 — first boot needs time to accept the QR scan; a 503 here
		// would make Fly kill the machine before the operator can link it.
		writeJSON(w, http.StatusOK, map[string]any{
			"logged_in": client.Store.ID != nil,
			"connected": client.IsConnected(),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// runLoginLoop drives the QR/login flow and keeps publishing fresh QR codes
// until the device is linked. Cycles indefinitely on timeout so the operator
// always has a scannable code at /qr.
func runLoginLoop(client *whatsmeow.Client, qr *qrHolder) {
	for {
		if client.Store.ID != nil {
			if !client.IsConnected() {
				if err := client.Connect(); err != nil {
					log.Warn("login_connect_failed", "error", err.Error(), "retry_in_seconds", 5)
					time.Sleep(5 * time.Second)
					continue
				}
				log.Info("login_connected_existing_session")
			}
			qr.set("")
			return
		}
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Warn("login_get_qr_channel_failed", "error", err.Error(), "retry_in_seconds", 5)
			time.Sleep(5 * time.Second)
			continue
		}
		if !client.IsConnected() {
			if err := client.Connect(); err != nil {
				log.Warn("login_connect_failed", "error", err.Error(), "retry_in_seconds", 5)
				time.Sleep(5 * time.Second)
				continue
			}
		}
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				qr.set(evt.Code)
				log.Info("qr_new_code_published")
			case "success":
				qr.set("")
				log.Info("login_success_device_linked")
				return
			case "timeout":
				log.Info("qr_channel_timed_out_reopening")
			default:
				log.Info("qr_event", "event", evt.Event)
			}
		}
	}
}

func qrHandler(client *whatsmeow.Client, qr *qrHolder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if client.Store.ID != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintln(w, "<h1>Already linked ✓</h1><p>No QR needed — bridge is logged in.</p>")
			return
		}
		code := qr.get()
		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "<h1>QR not ready yet</h1><p>Refresh in a few seconds.</p>")
			return
		}
		// PNG endpoint for img src
		if strings.HasSuffix(r.URL.Path, ".png") {
			png, err := qrcode.Encode(code, qrcode.Medium, 512)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(png)
			return
		}
		// HTML page that auto-refreshes every 20s (QR codes rotate).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<html><head><title>Link WhatsApp</title>
<meta http-equiv="refresh" content="20">
<style>body{font-family:system-ui;text-align:center;padding:2rem;background:#111;color:#eee}img{background:#fff;padding:1rem;border-radius:8px}</style>
</head><body>
<h1>Scan with WhatsApp → Linked Devices</h1>
<img src="/qr.png?t=%s" alt="qr" width="400">
<p style="color:#888">Code rotates every ~20s; the page refreshes automatically.</p>
</body></html>`, html.EscapeString(time.Now().Format("150405")))
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	storeDir := envOr("STORE_DIR", "./store")
	port := envOr("PORT", "8080")
	tokens := loadAuthTokens()
	if tokens == nil {
		log.Warn("auth_unauthenticated_mode", "note", "AUTH_TOKENS unset — dev only")
	} else {
		names := []string{}
		for _, c := range tokens {
			names = append(names, c)
		}
		log.Info("auth_configured", "caller_count", len(tokens), "callers", strings.Join(names, ","))
	}

	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		log.Error("store_dir_mkdir_failed", "path", storeDir, "error", err.Error())
		os.Exit(1)
	}
	dbPath := filepath.Join(storeDir, "whatsmeow.db")
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_pragma=busy_timeout(5000)", dbPath)

	waLogger := waLog.Stdout("whatsmeow", "INFO", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", dsn, waLogger)
	if err != nil {
		log.Error("sqlstore_new_failed", "error", err.Error())
		os.Exit(1)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Error("get_first_device_failed", "error", err.Error())
		os.Exit(1)
	}
	client := whatsmeow.NewClient(device, waLogger)

	qr := &qrHolder{}

	// Start HTTP immediately so /qr is reachable while we loop through QR codes.
	go runLoginLoop(client, qr)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", authMiddleware(tokens, sendHandler(client)))
	mux.HandleFunc("/health", healthHandler(client))
	mux.HandleFunc("/qr", qrHandler(client, qr))
	mux.HandleFunc("/qr.png", qrHandler(client, qr))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "whatsapp-notifier-bridge — see /qr, /health, /api/send")
	})

	addr := ":" + port
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("http_listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http_server_error", "error", err.Error())
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("shutdown_draining")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	client.Disconnect()
	if logCleanup != nil {
		logCleanup()
	}
}
