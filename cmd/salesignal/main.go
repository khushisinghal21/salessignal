// Command salesignal runs the webhook receiver, the rep/ops API and the demo UI.
//
// By default it runs in dry-run mode with demo chats loaded: nothing is sent
// to Meta. Set META_DATASET_ID and META_ACCESS_TOKEN to send for real.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/khushisinghal21/salessignal/internal/audit"
	"github.com/khushisinghal21/salessignal/internal/capi"
	"github.com/khushisinghal21/salessignal/internal/core"
	"github.com/khushisinghal21/salessignal/internal/demo"
	"github.com/khushisinghal21/salessignal/internal/outbox"
	"github.com/khushisinghal21/salessignal/internal/server"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	addr := env("ADDR", ":8080")
	datasetID, token := os.Getenv("META_DATASET_ID"), os.Getenv("META_ACCESS_TOKEN")
	live := datasetID != "" && token != ""
	demoOn := env("DEMO", strconv.FormatBool(!live)) == "true"
	valueCap, _ := strconv.ParseFloat(env("VALUE_CAP", "100000"), 64)

	cfg := core.Config{
		WABAID:          env("WABA_ID", "104729351122"),
		PageID:          env("PAGE_ID", "556677889900"),
		DefaultCurrency: env("DEFAULT_CURRENCY", "INR"),
		ValueCap:        valueCap,
		Reps:            strings.Split(env("REPS", "rep:alex,rep:priya"), ","),
	}

	build := func() *server.State {
		var sender capi.Sender
		var dry *capi.DryRunSender
		if live {
			sender = &capi.HTTPSender{DatasetID: datasetID, AccessToken: token}
		} else {
			dry = &capi.DryRunSender{}
			sender = dry
		}
		log := audit.New(nil)
		backoff := time.Second
		if live {
			backoff = 5 * time.Second
		}
		box := outbox.New(sender, outbox.Options{
			PartnerAgent: env("PARTNER_AGENT", "salesignal-poc"),
			BaseBackoff:  backoff,
			MaxBackoff:   30 * time.Second,
			OnTransition: func(it outbox.Item, from outbox.State) {
				d := map[string]any{"from": string(from), "attempts": it.Attempts, "path": it.Event.Path()}
				if it.LastError != "" {
					d["error"] = it.LastError
				}
				log.Append("outbox", "event_"+string(it.State), it.Meta["proposal"], d)
			},
		})
		svc := core.New(cfg, nil, log, box)
		if demoOn {
			demo.Seed(svc, time.Now())
		}
		return &server.State{Svc: svc, Box: box, DryRun: dry}
	}

	srv := server.New(server.Options{
		Build:       build,
		VerifyToken: os.Getenv("WEBHOOK_VERIFY_TOKEN"),
		AppSecret:   []byte(os.Getenv("META_APP_SECRET")),
		DemoEnabled: demoOn,
		Logger:      logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Deliver queued events in the background. Reading Current() on every tick
	// means a demo reset picks up the new outbox.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				box := srv.Current().Box
				for box.ProcessDue(ctx) > 0 {
				}
			}
		}
	}()

	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	mode := "dry-run (nothing is sent to Meta)"
	if live {
		mode = "LIVE: sending to Meta dataset " + datasetID
	}
	logger.Info("salesignal listening", "addr", addr, "mode", mode, "demo", demoOn)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
