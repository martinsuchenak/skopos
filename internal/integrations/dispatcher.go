package integrations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/ids"
)

// retrySchedule is the wait after each failed attempt (Standard Webhooks'
// suggestion): 8 attempts over about 27 hours, then the delivery fails.
var retrySchedule = []time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 5 * time.Hour, 10 * time.Hour, 10 * time.Hour}

const (
	attemptTimeout = 10 * time.Second
	// disableAfter: a subscription whose deliveries have all failed this
	// long is disabled.
	disableAfter = 72 * time.Hour
	// failureGap ends a run of failures: longer than the longest wait
	// between retries, so a still-failing endpoint is tried within it,
	// and a gap this long means nothing was failing in between.
	failureGap = 12 * time.Hour
	// perWebhook bounds one subscription's attempts per pass (all in
	// flight at once), so a slow endpoint holds a pass for one attempt
	// timeout at most and never queues the others behind its backlog.
	perWebhook  = 4
	fanOutBatch = 500 // events read per subscription per pass
	tick        = 2 * time.Second
)

// Logger is what the dispatcher logs through (serve's logger, slog).
type Logger interface {
	Warn(msg string, keysAndValues ...any)
}

// Dispatcher fans events out to subscriptions and delivers them.
type Dispatcher struct {
	svc    *Service
	client *http.Client
	log    Logger
}

func NewDispatcher(svc *Service, log Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{svc: svc, client: newClient(svc.policy), log: log}
}

// newClient refuses private addresses at connect time (after DNS, so a
// name can't resolve its way past the check), follows no redirects and
// uses no proxy.
func newClient(p URLPolicy) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !p.ipAllowed(ip) {
			return fmt.Errorf("%s is a private address (not in integrations.allow_private)", host)
		}
		return nil
	}}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: perWebhook,
		IdleConnTimeout:     time.Minute,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: tr, Timeout: attemptTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// Run delivers until ctx ends: on every emit, and every 2 seconds.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		// Taken before the pass: an event emitted during it wakes the
		// next one at once.
		var woken <-chan struct{}
		if d.svc.emitter != nil {
			woken = d.svc.emitter.Woken()
		}
		if err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.Warn("webhook delivery pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-woken:
		}
	}
}

// RunOnce fans new events out and makes the attempts that are due.
func (d *Dispatcher) RunOnce(ctx context.Context) error {
	if d.svc.key == nil {
		return nil
	}
	if err := d.fanOut(ctx); err != nil {
		return err
	}
	return d.deliverDue(ctx)
}

func (d *Dispatcher) fanOut(ctx context.Context) error {
	hooks, err := d.svc.load(ctx, "")
	if err != nil {
		return err
	}
	groups := groupCache{} // each group read once per pass
	for i := range hooks {
		w := &hooks[i]
		if !w.Enabled {
			continue // its events wait (up to the retention): re-enabling resumes from the cursor
		}
		if err := d.fanOutOne(ctx, w, groups); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) fanOutOne(ctx context.Context, w *Webhook, groups groupCache) error {
	rows, err := d.svc.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM integration_events WHERE seq > ? ORDER BY seq LIMIT ?`, w.cursor, fanOutBatch)
	if err != nil {
		return err
	}
	var evs []Stored
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		evs = append(evs, ev)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(evs) == 0 {
		return err
	}
	now := d.svc.now().UnixMilli()
	tx, err := d.svc.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, ev := range evs {
		if !d.svc.wants(ctx, w, ev, groups) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO webhook_deliveries (id, webhook_id, event_seq, item_id, next_ms, created_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			ids.New(), w.ID, ev.Seq, ev.ItemID, now, now); err != nil {
			return err
		}
	}
	// Advance only if the cursor is still where this pass read it.
	if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET cursor_seq = ? WHERE id = ? AND cursor_seq = ?`, evs[len(evs)-1].Seq, w.ID, w.cursor); err != nil {
		return err
	}
	return tx.Commit()
}

type due struct {
	id, webhookID string
	seq           int64
	attempts      int
}

func (d *Dispatcher) deliverDue(ctx context.Context) error {
	now := d.svc.now().UnixMilli()
	// Per item, in order: a delivery waits while an older one for the same
	// item and subscription hasn't had its first attempt. At most
	// perWebhook per subscription, oldest first.
	rows, err := d.svc.db.QueryContext(ctx, `
		SELECT id, webhook_id, event_seq, attempts FROM (
			SELECT dl.id, dl.webhook_id, dl.event_seq, dl.attempts,
				ROW_NUMBER() OVER (PARTITION BY dl.webhook_id ORDER BY dl.event_seq) AS rn
			FROM webhook_deliveries dl JOIN webhooks w ON w.id = dl.webhook_id
			WHERE dl.state = 'pending' AND dl.next_ms <= ? AND w.enabled = 1
			  AND NOT EXISTS (SELECT 1 FROM webhook_deliveries o WHERE o.webhook_id = dl.webhook_id AND dl.item_id <> ''
			      AND o.item_id = dl.item_id AND o.state = 'pending' AND o.attempts = 0 AND o.event_seq < dl.event_seq))
		WHERE rn <= ? ORDER BY event_seq`, now, perWebhook)
	if err != nil {
		return err
	}
	var batch []due
	for rows.Next() {
		var x due
		if err := rows.Scan(&x.id, &x.webhookID, &x.seq, &x.attempts); err != nil {
			_ = rows.Close()
			return err
		}
		batch = append(batch, x)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(batch) == 0 {
		return err
	}
	hooks := map[string]*Webhook{}
	var wg sync.WaitGroup
	for _, x := range batch {
		w, ok := hooks[x.webhookID]
		if !ok {
			loaded, err := d.svc.load(ctx, x.webhookID)
			if err != nil {
				return err
			}
			if len(loaded) == 0 {
				continue
			}
			w = &loaded[0]
			hooks[x.webhookID] = w
		}
		wg.Add(1)
		go func(x due, w *Webhook) {
			defer wg.Done()
			if err := d.attempt(ctx, w, x); err != nil && ctx.Err() == nil {
				d.log.Warn("webhook delivery attempt failed to record", "webhook", w.ID, "delivery", x.id, "error", err)
			}
		}(x, w)
	}
	wg.Wait()
	return nil
}

// attempt POSTs one delivery and records the outcome.
func (d *Dispatcher) attempt(ctx context.Context, w *Webhook, x due) error {
	row := d.svc.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM integration_events WHERE seq = ?`, x.seq)
	ev, err := scanEvent(row)
	if err != nil {
		if err == sql.ErrNoRows {
			_, err = d.svc.db.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'failed', last_error = 'the event is no longer kept' WHERE id = ?`, x.id)
		}
		return err
	}
	secrets, err := d.secrets(w)
	if err != nil {
		return d.finish(ctx, w, x, 0, err.Error(), 0)
	}
	body, err := json.Marshal(ev.Payload(d.svc.PublicURL))
	if err != nil {
		return err
	}
	ts := d.svc.now().Unix()
	start := time.Now() // the attempt's duration, on the wall clock
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return d.finish(ctx, w, x, 0, err.Error(), 0)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "skopos-webhooks/"+build.Version)
	req.Header.Set("webhook-id", ev.ID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", Sign(secrets, ev.ID, ts, body))
	resp, err := d.client.Do(req)
	took := time.Since(start).Milliseconds()
	if err != nil {
		return d.finish(ctx, w, x, 0, err.Error(), took)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return d.finish(ctx, w, x, resp.StatusCode, "", took)
}

func (d *Dispatcher) secrets(w *Webhook) ([][]byte, error) {
	cur, err := d.svc.open(w.secret)
	if err != nil {
		return nil, err
	}
	out := [][]byte{cur}
	if len(w.oldSecret) > 0 && d.svc.now().Before(w.oldUntil) {
		// The old secret only eases the switch-over: one sealed under an
		// earlier integrations key is left out, not a failed delivery.
		if old, err := d.svc.open(w.oldSecret); err == nil {
			out = append(out, old)
		}
	}
	return out, nil
}

// finish records an attempt: delivered on 2xx; on 410 the subscription is
// disabled; otherwise the next try is scheduled, or the delivery fails.
func (d *Dispatcher) finish(ctx context.Context, w *Webhook, x due, code int, errText string, took int64) error {
	now := d.svc.now().UTC()
	nowMS := now.UnixMilli()
	if len(errText) > 500 {
		errText = errText[:500]
	}
	if code != 0 && (code < 200 || code > 299) && errText == "" {
		errText = fmt.Sprintf("HTTP %d", code)
	}
	tx, err := d.svc.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Deleted with its subscription while the POST was in flight: nothing
	// to record.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_deliveries WHERE id = ?)`, x.id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_attempts (delivery_id, at_ms, code, error, duration_ms) VALUES (?, ?, ?, ?, ?)`,
		x.id, nowMS, code, errText, took); err != nil {
		return err
	}
	attempts := x.attempts + 1
	disable := ""
	switch {
	case code >= 200 && code <= 299:
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'delivered', attempts = ?, last_code = ?, last_error = '', last_ms = ? WHERE id = ?`,
			attempts, code, nowMS, x.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET failing_since_ms = NULL, last_failure_ms = NULL WHERE id = ?`, w.ID); err != nil {
			return err
		}
	default:
		state, next := StatePending, nowMS
		if code == http.StatusGone {
			state, disable = StateFailed, "the endpoint answered 410 Gone"
		} else if attempts > len(retrySchedule) {
			state = StateFailed
		} else {
			next = now.Add(retrySchedule[attempts-1]).UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state = ?, attempts = ?, next_ms = ?, last_code = ?, last_error = ?, last_ms = ? WHERE id = ?`,
			state, attempts, next, code, errText, nowMS, x.id); err != nil {
			return err
		}
		// A failure after a quiet gap starts a new run: idle days aren't
		// failing days.
		if _, err := tx.ExecContext(ctx, `
			UPDATE webhooks SET
				failing_since_ms = CASE WHEN failing_since_ms IS NULL OR last_failure_ms IS NULL OR last_failure_ms < ? THEN ? ELSE failing_since_ms END,
				last_failure_ms = ?
			WHERE id = ?`, nowMS-failureGap.Milliseconds(), nowMS, nowMS, w.ID); err != nil {
			return err
		}
		var since int64
		if err := tx.QueryRowContext(ctx, `SELECT failing_since_ms FROM webhooks WHERE id = ?`, w.ID).Scan(&since); err != nil {
			return err
		}
		if disable == "" && nowMS-since >= disableAfter.Milliseconds() {
			disable = "every delivery failed for 3 days (last: " + errText + ")"
		}
		if disable != "" {
			res, err := tx.ExecContext(ctx, `UPDATE webhooks SET enabled = 0, disabled_reason = ?, updated_ms = ? WHERE id = ? AND enabled = 1`,
				disable, nowMS, w.ID)
			if err != nil {
				return err
			}
			// Several attempts of one pass can reach the rule: only the one
			// that disabled it logs and audits.
			if n, _ := res.RowsAffected(); n == 0 {
				disable = ""
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if disable != "" {
		d.log.Warn("webhook disabled", "webhook", w.ID, "name", w.Name, "reason", disable)
		d.svc.record(context.WithoutCancel(ctx), w, "webhook.disable", audit.ViaSystem, disable)
		if d.svc.onDisable != nil {
			d.svc.onDisable(context.WithoutCancel(ctx), *w, disable)
		}
	}
	return nil
}

// Prune drops what retention allows (the cleanup ticker): finished
// deliveries older than cutoff, and events older than cutoff that no
// enabled subscription still has to fan out or deliver.
func Prune(ctx context.Context, db *sql.DB, cutoff time.Time) error {
	ms := cutoff.UnixMilli()
	if _, err := db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE state <> 'pending' AND created_ms < ?`, ms); err != nil {
		return fmt.Errorf("pruning webhook deliveries: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM integration_events WHERE occurred_ms < ?
		  AND seq <= COALESCE((SELECT MIN(cursor_seq) FROM webhooks WHERE enabled = 1), seq)
		  AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.event_seq = integration_events.seq AND d.state = 'pending')`, ms); err != nil {
		return fmt.Errorf("pruning integration events: %w", err)
	}
	return nil
}
