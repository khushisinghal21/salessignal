// Package server exposes the WhatsApp webhook, the rep/ops API and the demo UI.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/khushisinghal21/salessignal/internal/capi"
	"github.com/khushisinghal21/salessignal/internal/core"
	"github.com/khushisinghal21/salessignal/internal/outbox"
	"github.com/khushisinghal21/salessignal/internal/whatsapp"
)

//go:embed web/index.html
var indexHTML []byte

// State is one running instance: the service, its outbox, and (in dry-run
// mode) the fake Meta endpoint.
type State struct {
	Svc    *core.Service
	Box    *outbox.Outbox
	DryRun *capi.DryRunSender // nil when sending to Meta for real
}

type Options struct {
	Build       func() *State // makes a fresh State; used at start and on demo reset
	VerifyToken string        // for Meta's webhook verification handshake
	AppSecret   []byte        // checks X-Hub-Signature-256 when set
	DemoEnabled bool
	Logger      *slog.Logger
}

type Server struct {
	opt Options
	mu  sync.RWMutex
	st  *State
}

func New(opt Options) *Server {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Server{opt: opt, st: opt.Build()}
}

func (s *Server) Current() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /webhook", s.verifyWebhook)
	mux.HandleFunc("POST /webhook", s.receiveWebhook)

	mux.HandleFunc("GET /api/state", s.withActor(s.state))
	mux.HandleFunc("GET /api/proposals", s.withActor(func(w http.ResponseWriter, r *http.Request, actor string) {
		writeJSON(w, 200, s.Current().Svc.Proposals(actor, core.Status(r.URL.Query().Get("status"))))
	}))
	mux.HandleFunc("POST /api/proposals/{id}/confirm", s.withActor(s.confirm))
	mux.HandleFunc("POST /api/proposals/{id}/reject", s.withActor(s.reject))
	mux.HandleFunc("POST /api/conversations/{id}/outbound", s.withActor(s.outbound))
	mux.HandleFunc("GET /api/conversations", s.withActor(func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, 200, s.Current().Svc.Conversations())
	}))
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.Current().Svc.Stats()) })
	mux.HandleFunc("GET /api/outbox", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.Current().Svc.Outbox()) })
	mux.HandleFunc("GET /api/sweep", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.Current().Svc.Sweep()) })
	mux.HandleFunc("GET /api/audit", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, 200, s.Current().Svc.Audit(limit))
	})

	if s.opt.DemoEnabled {
		mux.HandleFunc("POST /api/demo/reset", func(w http.ResponseWriter, r *http.Request) {
			st := s.opt.Build()
			s.mu.Lock()
			s.st = st
			s.mu.Unlock()
			writeJSON(w, 200, map[string]string{"status": "reset"})
		})
		mux.HandleFunc("POST /api/demo/outage", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				On bool `json:"on"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
				writeErr(w, 400, "body must be {\"on\": true|false}")
				return
			}
			if d := s.Current().DryRun; d != nil {
				d.SetFailing(in.On)
			}
			writeJSON(w, 200, map[string]bool{"outage": in.On})
		})
	}
	return logRequests(s.opt.Logger, mux)
}

// verifyWebhook answers Meta's subscription handshake.
func (s *Server) verifyWebhook(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if s.opt.VerifyToken == "" || q.Get("hub.mode") != "subscribe" || q.Get("hub.verify_token") != s.opt.VerifyToken {
		writeErr(w, 403, "verification failed")
		return
	}
	w.Write([]byte(q.Get("hub.challenge")))
}

func (s *Server) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "could not read body")
		return
	}
	if len(s.opt.AppSecret) > 0 && !whatsapp.VerifySignature(s.opt.AppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeErr(w, 401, "bad signature")
		return
	}
	var p whatsapp.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, 400, "not a WhatsApp webhook payload")
		return
	}
	n := s.Current().Svc.Ingest(p)
	writeJSON(w, 200, map[string]int{"messages": n})
}

func (s *Server) withActor(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("X-Actor")
		if actor == "" {
			actor = r.URL.Query().Get("actor")
		}
		if actor != core.Admin && !slices.Contains(s.Current().Svc.Config().Reps, actor) {
			writeErr(w, 401, "set X-Actor to a rep (e.g. rep:alex) or admin")
			return
		}
		h(w, r, actor)
	}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request, actor string) {
	st := s.Current()
	resp := map[string]any{
		"actor":         actor,
		"reps":          st.Svc.Config().Reps,
		"proposals":     st.Svc.Proposals(actor, ""),
		"conversations": st.Svc.Conversations(),
		"stats":         st.Svc.Stats(),
		"outbox":        st.Svc.Outbox(),
		"nudges":        st.Svc.Sweep(),
		"audit":         st.Svc.Audit(14),
		"demo":          s.opt.DemoEnabled,
		"dry_run":       st.DryRun != nil,
	}
	if st.DryRun != nil {
		resp["outage"] = st.DryRun.Failing()
		if req, ok := st.DryRun.Last(); ok {
			resp["last_request"] = req
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, actor string) {
	var in core.ConfirmInput
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, 400, "body must be {\"value\": number, \"currency\": \"INR\", \"force\": bool}")
			return
		}
	}
	p, rep, err := s.Current().Svc.Confirm(r.PathValue("id"), actor, in)
	if err != nil {
		code := statusFor(err)
		writeJSON(w, code, map[string]any{"error": err.Error(), "gate": rep, "proposal": p})
		return
	}
	writeJSON(w, 200, map[string]any{"proposal": p, "gate": rep})
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in)
	p, err := s.Current().Svc.Reject(r.PathValue("id"), actor, in.Reason)
	if err != nil {
		writeJSON(w, statusFor(err), map[string]any{"error": err.Error(), "proposal": p})
		return
	}
	writeJSON(w, 200, map[string]any{"proposal": p})
}

func (s *Server) outbound(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&in); err != nil || in.Text == "" {
		writeErr(w, 400, "body must be {\"text\": \"...\"}")
		return
	}
	if err := s.Current().Svc.AddOutbound(r.PathValue("id"), in.Text, time.Time{}); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "recorded"})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return 404
	case errors.Is(err, core.ErrForbidden):
		return 403
	case errors.Is(err, core.ErrNotOpen), errors.Is(err, core.ErrDuplicate):
		return 409
	case errors.Is(err, core.ErrNeedValue), errors.Is(err, core.ErrGate):
		return 422
	}
	return 500
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

// logRequests logs method, path and status. It never logs bodies or query
// strings, which can carry phone numbers or tokens.
func logRequests(l *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/api/state" {
			l.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "ms", time.Since(start).Milliseconds())
		}
	})
}
