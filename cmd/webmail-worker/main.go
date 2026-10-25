// Command webmail-worker runs the scheduled and deferred work that must not
// happen in a request: draining the federation outbox, and sweeping the state
// that ages out with it.
//
// It is a separate binary from the API server on purpose. Delivery to a peer is
// a network call to a machine we do not control, with a 72-hour retry budget;
// putting that behind a user's compose would mean the compose waits for a peer
// that may be down. See ADR 17 and principle 5.
//
// Running several instances is safe. Rows are leased to one worker before any
// network work, so two workers never send the same envelope, and delivery is
// idempotent at the receiver anyway. A worker that dies loses its leases when
// they expire, so its rows need no one to put them back (ADR 19).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/calendar"
	"github.com/103mail/webmail-server/internal/chat"
	"github.com/103mail/webmail-server/internal/config"
	"github.com/103mail/webmail-server/internal/email"
	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/notify"
	"github.com/103mail/webmail-server/internal/store"

	"path/filepath"
)

const (
	defaultInterval = 5 * time.Second
	// chatInterval is how often the chat loop drains, which is how long a
	// chat message waits at most before it goes out (RFC 14.4).
	chatInterval = time.Second
	// batch is how many envelopes are claimed per peer per pass. Small, because
	// a pass holds no locks across the network and a short cycle keeps a
	// recovering peer's queue moving without a thundering herd.
	batch = 20
	// sweepEvery is how often aged-out dedup, nonce and delivered-outbox rows
	// are deleted. There is no hurry: retention is measured in days.
	sweepEvery = 15 * time.Minute
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
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

	startup, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	if err := db.PingContext(startup); err != nil {
		return err
	}

	fedConfig, err := federation.LoadConfig()
	if err != nil {
		return err
	}
	fed, err := federation.New(db, fedConfig, log, nil)
	if err != nil {
		return err
	}
	if !fed.Enabled() {
		// Nothing to drain and nothing that could ever appear to drain. Exiting
		// rather than idling makes a misconfiguration visible instead of
		// looking like a healthy worker with no work.
		log.Error("no federation config; this worker has nothing to do")
		return nil
	}

	// The worker needs the mail domain for one thing only: writing a bounce
	// into the sender's mailbox when delivery permanently fails.
	attachments := blob.New(filepath.Join(cfg.DataDir, "attachments"))
	mail := email.New(db, attachments, notify.New(db, nil), fed, log)
	fed.Attach(mail)
	// And the chat domain, to record what peers accepted and to tell a sender
	// in the chat when a message did not arrive.
	fed.AttachChat(chat.New(db, attachments))
	// And the calendar domain, to drop attendees a peer refused or that are
	// on a Domain without calendar, with a new revision, and to tell the
	// organizer (RFC 15.1, 15.4).
	meetings := calendar.New(db, blob.New(filepath.Join(cfg.DataDir, "documents")))
	meetings.NotifyBy(mail)
	meetings.Federate(fed)
	fed.AttachCalendar(meetings)

	interval := defaultInterval
	if v := os.Getenv("WEBMAIL_WORKER_INTERVAL_SECONDS"); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil || seconds <= 0 {
			log.Warn("ignoring WEBMAIL_WORKER_INTERVAL_SECONDS", "value", v)
		} else {
			interval = time.Duration(seconds) * time.Second
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("worker started", "domains", fed.LocalDomains(), "interval", interval.String())

	drain := time.NewTicker(interval)
	defer drain.Stop()
	sweep := time.NewTicker(sweepEvery)
	defer sweep.Stop()

	// Chat has a loop of its own, so a mail pass stuck on a slow peer never
	// holds a chat message up (RFC 14.4).
	go chatLoop(ctx, fed, log)

	// One pass immediately, so a restart does not wait a full interval before
	// picking up whatever is due.
	drainOnce(ctx, fed, log)

	for {
		select {
		case <-ctx.Done():
			log.Info("worker shutting down")
			return nil
		case <-drain.C:
			drainOnce(ctx, fed, log)
		case <-sweep.C:
			if err := fed.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.Error("sweep failed", "error", err)
			}
		}
	}
}

// chatLoop drains the chat envelopes and read receipts every second until the
// worker stops.
func chatLoop(ctx context.Context, fed *federation.Service, log *slog.Logger) {
	tick := time.NewTicker(chatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pass, cancel := context.WithTimeout(ctx, time.Minute)
			handled, err := fed.DrainChat(pass, batch)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("chat drain failed", "error", err)
			}
			if handled > 0 {
				log.Info("drained the chat outbox", "envelopes", handled)
			}
		}
	}
}

// drainOnce makes one pass over the due work. A pass is bounded by its own
// context so that a peer that accepts a connection and then goes quiet cannot
// stall the loop past the next tick by more than the client timeout.
func drainOnce(ctx context.Context, fed *federation.Service, log *slog.Logger) {
	pass, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	handled, err := fed.Drain(pass, batch)
	if err != nil && ctx.Err() == nil {
		log.Error("drain failed", "error", err)
	}
	if handled > 0 {
		log.Info("drained the outbox", "envelopes", handled)
	}
}
