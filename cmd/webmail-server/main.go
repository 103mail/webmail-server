// Command webmail-server serves the webmail JSON API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/103mail/webmail-server/internal/api"
	"github.com/103mail/webmail-server/internal/auth"
	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/calendar"
	"github.com/103mail/webmail-server/internal/chat"
	"github.com/103mail/webmail-server/internal/config"
	"github.com/103mail/webmail-server/internal/email"
	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/mailer"
	"github.com/103mail/webmail-server/internal/notify"
	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/wsbridge"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// setupOutbound builds the outbound path. Without a topic ARN the server stays
// off AWS and records notifications in memory instead of publishing them, so a
// development or staging deployment cannot mail real people.
func setupOutbound(ctx context.Context, cfg *config.Config, log *slog.Logger) (mailer.Sender, error) {
	if cfg.OutboundTopicARN == "" {
		log.Warn("no SNS topic configured; outbound notifications are recorded in memory")
		return mailer.NewRecorder(), nil
	}
	return mailer.LoadTopic(ctx, mailer.TopicConfig{
		Region:   cfg.SNSRegion,
		TopicARN: cfg.OutboundTopicARN,
	})
}

// setupInbound builds the inbound path: the S3 store an SES receipt rule writes
// to, and the verifier that authenticates the SNS post announcing it. Without a
// region both are nil and the webhook reports that it is unavailable rather
// than accepting posts it cannot authenticate.
func setupInbound(ctx context.Context, cfg *config.Config, log *slog.Logger) (
	*mailer.Receiver, *mailer.Verifier, error) {
	if cfg.SESRegion == "" {
		log.Warn("no SES region configured; inbound mail is disabled")
		return nil, nil, nil
	}
	objects, err := mailer.LoadS3Objects(ctx, cfg.SESRegion, 0)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.InboundTopicARNs) == 0 {
		log.Warn("no SNS topic ARNs configured; the inbound webhook accepts any AWS-signed topic")
	}
	verifier := mailer.NewVerifier(nil, cfg.InboundTopicARNs...)
	return mailer.NewReceiver(verifier, objects), verifier, nil
}

// setupFederation builds the federation service. Without a configuration file
// it returns a disabled service rather than nil, so every caller can treat
// "not federated" as "every address is local" without a nil check.
func setupFederation(ctx context.Context, db *store.DB, log *slog.Logger) (*federation.Service, error) {
	cfg, err := federation.LoadConfig()
	if err != nil {
		return nil, err
	}
	fed, err := federation.New(db, cfg, log, nil)
	if err != nil {
		return nil, err
	}
	if !fed.Enabled() {
		log.Warn("no federation config; every recipient is treated as local")
		return fed, nil
	}
	if err := fed.RegisterLocalDomains(ctx); err != nil {
		return nil, err
	}
	log.Info("federation enabled", "domains", fed.LocalDomains())
	return fed, nil
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DSN)
	if err != nil {
		return err
	}
	defer db.Close()
	// A test stack lets time pass by moving this file forward rather than
	// waiting (store.OffsetClock). Production never sets it.
	if path := os.Getenv("WEBMAIL_CLOCK_FILE"); path != "" {
		db.Clock = store.OffsetClock(path)
		log.Warn("the clock runs ahead by the offset in a file, for testing only", "file", path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// An empty database gets the schema, which is what lets a new Domain start
	// from nothing but an SQLite file name (ADR 19). An existing one is left
	// alone; schema changes are migrations.
	created, err := db.CreateSchema(ctx)
	if err != nil {
		return err
	}
	if created {
		log.Info("created the database schema", "engine", db.Engine)
	}

	attachments := blob.New(filepath.Join(cfg.DataDir, "attachments"))
	documents := blob.New(filepath.Join(cfg.DataDir, "documents"))

	sender, err := setupOutbound(ctx, cfg, log)
	if err != nil {
		return err
	}
	receiver, verifier, err := setupInbound(ctx, cfg, log)
	if err != nil {
		return err
	}
	// notify is the only outbound path, and it can only send an
	// email_template row. See ADR 4 and ADR 16.
	notifier := notify.New(db, sender)

	// Federation is wired in two directions and neither package imports the
	// other: the mail domain enqueues outbound envelopes through the federation
	// service, and the federation service hands accepted envelopes back to the
	// mail domain through an interface it declares. See ADR 17.
	fed, err := setupFederation(ctx, db, log)
	if err != nil {
		return err
	}
	var federator email.Federator
	if fed.Enabled() {
		federator = fed
	}
	mail := email.New(db, attachments, notifier, federator, log)
	fed.Attach(mail)

	chats := chat.New(db, attachments)

	server := api.New(cfg, log,
		auth.New(db),
		mail,
		calendar.New(db, documents),
		chats,
		receiver, verifier, fed,
		// The chat WebSocket bridge. Connections terminate in ws2api, so this
		// holds none of them; it answers the proxy's HTTP calls and pushes
		// back through it. See ADR 13 and ADR 18.
		wsbridge.New(chats, log),
	)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errs := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	// The end-to-end test controls get a listener of their own, so they can
	// never be reached through the public hostname (ADR 20).
	var controlServer *http.Server
	if cfg.ControlAddr != "" {
		controlServer = &http.Server{
			Addr:              cfg.ControlAddr,
			Handler:           server.ControlHandler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Warn("serving the end-to-end test controls", "addr", cfg.ControlAddr)
			if err := controlServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errs:
		return err
	case <-stop:
		log.Info("shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	if controlServer != nil {
		controlServer.Shutdown(shutdownCtx)
	}
	return httpServer.Shutdown(shutdownCtx)
}
