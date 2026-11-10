package integrations

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

var (
	ErrInvalidInput = errors.New("invalid webhook input")
	ErrNotFound     = errors.New("not found")
	// ErrNoKey: the server has no integrations key, so it can't keep
	// webhook secrets.
	ErrNoKey = errors.New("webhooks need the server's integrations key (integrations.secret_key / SKOPOS_INTEGRATIONS_KEY)")
)

// Webhook is a subscription (its secret never leaves storage after the
// create or rotate answer).
type Webhook struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	URL            string     `json:"url"`
	Workspaces     []string   `json:"workspaces"`
	Groups         []string   `json:"groups"`
	Types          []string   `json:"types"`
	Enabled        bool       `json:"enabled"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	FailingSince   *time.Time `json:"failing_since,omitempty"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	secret, oldSecret []byte
	oldUntil          time.Time
	cursor            int64
}

// WithSecret is the create and rotate answer: the secret, shown once.
type WithSecret struct {
	Webhook Webhook `json:"webhook"`
	Secret  string  `json:"secret"`
}

// Input creates or changes a subscription; nil fields are unchanged on an
// update.
type Input struct {
	Name       *string   `json:"name,omitempty"`
	URL        *string   `json:"url,omitempty"`
	Workspaces *[]string `json:"workspaces,omitempty"`
	Groups     *[]string `json:"groups,omitempty"`
	Types      *[]string `json:"types,omitempty"`
	Enabled    *bool     `json:"enabled,omitempty"`
	Via        string    `json:"-"`
}

// Delivery is one event to one subscription, with its attempts.
type Delivery struct {
	ID        string     `json:"id"`
	EventID   string     `json:"event_id"`
	EventType string     `json:"event_type"`
	EventSeq  int64      `json:"event_seq"`
	ItemID    string     `json:"item_id,omitempty"`
	State     string     `json:"state"`
	Attempts  int        `json:"attempts"`
	NextAt    *time.Time `json:"next_at,omitempty"`
	LastCode  int        `json:"last_code,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	Log       []Attempt  `json:"attempts_log,omitempty"`
}

// Attempt is one POST.
type Attempt struct {
	At         time.Time `json:"at"`
	Code       int       `json:"code,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

// Delivery states.
const (
	StatePending   = "pending"
	StateDelivered = "delivered"
	StateFailed    = "failed"
)

// rotateGrace: after a rotation, deliveries are signed with both secrets
// this long.
const rotateGrace = 24 * time.Hour

// AuditRecorder writes the webhook.* entries (internal/audit).
type AuditRecorder interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// Service manages subscriptions and serves the feed.
type Service struct {
	db      *sql.DB
	key     []byte // AES-256 key from the integrations key; nil = no webhooks
	audit   AuditRecorder
	emitter *Emitter
	policy  URLPolicy
	// GroupScope reads a workspace group's members and path.Match
	// patterns; nil = groups unsupported.
	GroupScope func(ctx context.Context, group string) (members, patterns []string, err error)
	// GroupExists resolves a group name or id to its id ("" = none).
	GroupExists func(ctx context.Context, group string) string
	PublicURL   string
	now         func() time.Time
	// onDisable is told when the dispatcher disables a subscription (the
	// Slack relay posts it); nil = nobody.
	onDisable func(ctx context.Context, w Webhook, reason string)
}

// SetDisableNotifier wires the alert for a subscription the dispatcher
// disabled (410 Gone, or 3 days of failures).
func (s *Service) SetDisableNotifier(fn func(ctx context.Context, w Webhook, reason string)) {
	s.onDisable = fn
}

// NewService: secretKey is the server's integrations key ("" = webhooks
// off; the feed still works).
func NewService(db *sql.DB, emitter *Emitter, secretKey string, policy URLPolicy) *Service {
	s := &Service{db: db, emitter: emitter, policy: policy, now: time.Now}
	if secretKey != "" {
		sum := sha256.Sum256([]byte(secretKey))
		s.key = sum[:]
	}
	return s
}

func (s *Service) SetAuditRecorder(a AuditRecorder) { s.audit = a }

// --- secrets ---

func newSecret() ([]byte, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	return b, "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}

func (s *Service) seal(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (s *Service) open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	if s.key == nil {
		return nil, ErrNoKey
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("webhook secret: corrupt")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("webhook secret: cannot decrypt (was the integrations key changed?)")
	}
	return plain, nil
}

// Sign is the Standard Webhooks signature header value: "v1,<base64
// HMAC-SHA256(secret, id.timestamp.body)>", one per secret, space-separated.
func Sign(secrets [][]byte, msgID string, ts int64, body []byte) string {
	var sigs []string
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(msgID + "." + strconv.FormatInt(ts, 10) + "."))
		mac.Write(body)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(sigs, " ")
}

// --- validation and access ---

// URLPolicy says which webhook URLs are allowed: https anywhere public;
// http only to AllowHTTP hosts; private, loopback and link-local addresses
// only inside AllowPrivate.
type URLPolicy struct {
	AllowHTTP    []string
	AllowPrivate []*net.IPNet
}

// ParseCIDRs reads integrations.allow_private.
func ParseCIDRs(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range list {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				c = fmt.Sprintf("%s/%d", c, bits)
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("integrations allow_private: %q is not a CIDR", c)
		}
		out = append(out, n)
	}
	return out, nil
}

const maxURL = 2000

func (p URLPolicy) checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || len(raw) > maxURL || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%w: url must be an absolute http(s) URL without credentials", ErrInvalidInput)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !slices.Contains(p.AllowHTTP, strings.ToLower(u.Hostname())) {
			return "", fmt.Errorf("%w: url must be https (http only to hosts in integrations.allow_http)", ErrInvalidInput)
		}
	default:
		return "", fmt.Errorf("%w: url must be https", ErrInvalidInput)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !p.ipAllowed(ip) {
		return "", fmt.Errorf("%w: %s is a private address; add it to integrations.allow_private", ErrInvalidInput, ip)
	}
	return raw, nil
}

// ipAllowed: public addresses always; loopback, private, link-local,
// CGNAT, unspecified and multicast only inside AllowPrivate.
func (p URLPolicy) ipAllowed(ip net.IP) bool {
	for _, n := range p.AllowPrivate {
		if n.Contains(ip) {
			return true
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 { // 100.64.0.0/10
		return false
	}
	return true
}

func cleanList(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Service) checkTypes(types []string) ([]string, error) {
	types = cleanList(types)
	for _, t := range types {
		if !knownType(t) {
			return nil, fmt.Errorf("%w: unknown event type %q (known: %s)", ErrInvalidInput, t, strings.Join(Types, ", "))
		}
	}
	return types, nil
}

// mayManage: root for any subscription; an approver for one whose
// workspaces are all in scope and that names no group (groups are root's).
func mayManage(ctx context.Context, workspaces, groups []string) error {
	p := auth.PrincipalFromContext(ctx)
	if p.IsRoot() {
		return nil
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return err
	}
	if len(groups) > 0 {
		return fmt.Errorf("%w: subscribing a workspace group needs the root key", auth.ErrRootRequired)
	}
	for _, ws := range workspaces {
		if !p.CanAccess(ws) {
			return fmt.Errorf("%w: %q", auth.ErrOutOfScope, ws)
		}
	}
	return nil
}

// --- management ---

// Create adds a subscription and returns its secret, once.
func (s *Service) Create(ctx context.Context, in Input) (*WithSecret, error) {
	if s.key == nil {
		return nil, ErrNoKey
	}
	w := Webhook{Enabled: true}
	if in.Name != nil {
		w.Name = strings.TrimSpace(*in.Name)
	}
	if w.Name == "" || len(w.Name) > 100 {
		return nil, fmt.Errorf("%w: name is required (at most 100 characters)", ErrInvalidInput)
	}
	if in.URL == nil {
		return nil, fmt.Errorf("%w: url is required", ErrInvalidInput)
	}
	var err error
	if w.URL, err = s.policy.checkURL(*in.URL); err != nil {
		return nil, err
	}
	if in.Workspaces != nil {
		w.Workspaces = cleanList(*in.Workspaces)
	}
	if in.Groups != nil {
		if w.Groups, err = s.resolveGroups(ctx, *in.Groups); err != nil {
			return nil, err
		}
	}
	if len(w.Workspaces) == 0 && len(w.Groups) == 0 {
		return nil, fmt.Errorf("%w: name the workspaces (or root: groups) whose events it gets", ErrInvalidInput)
	}
	if w.Types = []string{}; in.Types != nil {
		if w.Types, err = s.checkTypes(*in.Types); err != nil {
			return nil, err
		}
	}
	if err := mayManage(ctx, w.Workspaces, w.Groups); err != nil {
		return nil, err
	}
	secret, shown, err := newSecret()
	if err != nil {
		return nil, err
	}
	sealed, err := s.seal(secret)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	w.ID, w.CreatedAt, w.UpdatedAt = ids.New(), now, now
	w.CreatedBy, _ = auth.Actor(ctx)
	// A new subscription starts at the current end of the log: it gets
	// what happens from now on.
	var cursor int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM integration_events`).Scan(&cursor); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhooks (id, name, url, workspaces, groups, types, secret_enc, enabled, created_by, created_ms, updated_ms, cursor_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		w.ID, w.Name, w.URL, jsonList(w.Workspaces), jsonList(w.Groups), jsonList(w.Types), sealed, w.CreatedBy,
		now.UnixMilli(), now.UnixMilli(), cursor); err != nil {
		return nil, fmt.Errorf("storing webhook: %w", err)
	}
	s.record(ctx, &w, "webhook.create", in.Via, w.Name+" → "+w.URL)
	return &WithSecret{Webhook: w, Secret: shown}, nil
}

func (s *Service) resolveGroups(ctx context.Context, groups []string) ([]string, error) {
	groups = cleanList(groups)
	if len(groups) == 0 {
		return groups, nil
	}
	if s.GroupExists == nil || s.GroupScope == nil {
		return nil, fmt.Errorf("%w: workspace groups are not available", ErrInvalidInput)
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		id := s.GroupExists(ctx, g)
		if id == "" {
			return nil, fmt.Errorf("%w: no workspace group %q", ErrInvalidInput, g)
		}
		out = append(out, id)
	}
	return out, nil
}

// Update changes a subscription. Re-enabling clears the failure mark;
// pending deliveries continue and events since it was disabled follow.
func (s *Service) Update(ctx context.Context, id string, in Input) (*Webhook, error) {
	w, err := s.manageable(ctx, id)
	if err != nil {
		return nil, err
	}
	var changed []string
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 100 {
			return nil, fmt.Errorf("%w: name must be 1 to 100 characters", ErrInvalidInput)
		}
		if name != w.Name {
			w.Name, changed = name, append(changed, "name")
		}
	}
	if in.URL != nil {
		u, err := s.policy.checkURL(*in.URL)
		if err != nil {
			return nil, err
		}
		if u != w.URL {
			w.URL, changed = u, append(changed, "url")
		}
	}
	// A field counts as changed only when its value differs: the dashboard
	// sends every field on each save.
	if in.Workspaces != nil {
		if next := cleanList(*in.Workspaces); !sameSet(next, w.Workspaces) {
			w.Workspaces, changed = next, append(changed, "workspaces")
		}
	}
	if in.Groups != nil {
		next, err := s.resolveGroups(ctx, *in.Groups)
		if err != nil {
			return nil, err
		}
		if !sameSet(next, w.Groups) {
			w.Groups, changed = next, append(changed, "groups")
		}
	}
	if len(w.Workspaces) == 0 && len(w.Groups) == 0 {
		return nil, fmt.Errorf("%w: a webhook needs workspaces or groups", ErrInvalidInput)
	}
	if in.Types != nil {
		next, err := s.checkTypes(*in.Types)
		if err != nil {
			return nil, err
		}
		if !sameSet(next, w.Types) {
			w.Types, changed = next, append(changed, "types")
		}
	}
	// The new scope must be the caller's too.
	if err := mayManage(ctx, w.Workspaces, w.Groups); err != nil {
		return nil, err
	}
	action := "webhook.update"
	if in.Enabled != nil && *in.Enabled != w.Enabled {
		w.Enabled = *in.Enabled
		if w.Enabled {
			action, w.DisabledReason, w.FailingSince = "webhook.enable", "", nil // the failure run ends with it
		} else {
			action, w.DisabledReason = "webhook.disable", "disabled by "+actorName(ctx)
		}
	}
	now := s.now().UTC()
	var failing any
	if w.FailingSince != nil {
		failing = w.FailingSince.UnixMilli()
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE webhooks SET name = ?, url = ?, workspaces = ?, groups = ?, types = ?, enabled = ?, disabled_reason = ?,
			failing_since_ms = ?, updated_ms = ? WHERE id = ?`,
		w.Name, w.URL, jsonList(w.Workspaces), jsonList(w.Groups), jsonList(w.Types), boolInt(w.Enabled), w.DisabledReason,
		failing, now.UnixMilli(), w.ID); err != nil {
		return nil, fmt.Errorf("updating webhook: %w", err)
	}
	w.UpdatedAt = now
	if action != "webhook.update" || len(changed) > 0 {
		s.record(ctx, w, action, in.Via, strings.Join(changed, ", "))
	}
	if w.Enabled {
		s.emitter.wakeIfAny()
	}
	return w, nil
}

func actorName(ctx context.Context) string {
	a, _ := auth.Actor(ctx)
	return a
}

// Rotate replaces the secret; the old one keeps signing for 24 hours.
func (s *Service) Rotate(ctx context.Context, id, via string) (*WithSecret, error) {
	w, err := s.manageable(ctx, id)
	if err != nil {
		return nil, err
	}
	secret, shown, err := newSecret()
	if err != nil {
		return nil, err
	}
	sealed, err := s.seal(secret)
	if err != nil {
		return nil, err
	}
	var old []byte
	if err := s.db.QueryRowContext(ctx, `SELECT secret_enc FROM webhooks WHERE id = ?`, id).Scan(&old); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE webhooks SET secret_enc = ?, old_secret_enc = ?, old_until_ms = ?, updated_ms = ? WHERE id = ?`,
		sealed, old, now.Add(rotateGrace).UnixMilli(), now.UnixMilli(), id); err != nil {
		return nil, fmt.Errorf("rotating webhook secret: %w", err)
	}
	s.record(ctx, w, "webhook.rotate", via, "the old secret also signs until "+now.Add(rotateGrace).Format(time.RFC3339))
	w.UpdatedAt = now
	return &WithSecret{Webhook: *w, Secret: shown}, nil
}

// Delete removes a subscription and its deliveries.
func (s *Service) Delete(ctx context.Context, id, via string) error {
	w, err := s.manageable(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting webhook: %w", err)
	}
	s.record(ctx, w, "webhook.delete", via, w.Name)
	return nil
}

// Test sends a webhook.test event to this subscription only.
func (s *Service) Test(ctx context.Context, id, via string) (*Delivery, error) {
	w, err := s.manageable(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	msgID := "msg_" + ids.New()
	data, _ := json.Marshal(map[string]any{"webhook_id": w.ID, "message": "test event from skopos"})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO integration_events (id, type, occurred_ms, data) VALUES (?, ?, ?, ?)`,
		msgID, TypeWebhookTest, now.UnixMilli(), string(data))
	if err != nil {
		return nil, err
	}
	seq, _ := res.LastInsertId()
	d := Delivery{ID: ids.New(), EventID: msgID, EventType: TypeWebhookTest, EventSeq: seq, State: StatePending, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries (id, webhook_id, event_seq, next_ms, created_ms) VALUES (?, ?, ?, ?, ?)`,
		d.ID, w.ID, seq, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.record(ctx, w, "webhook.test", via, "")
	s.emitter.wakeIfAny()
	return &d, nil
}

// Redeliver puts deliveries back on a fresh schedule, keeping their
// webhook-id: one by id, or every failed one since a time.
func (s *Service) Redeliver(ctx context.Context, id, deliveryID string, since time.Time, via string) (int64, error) {
	w, err := s.manageable(ctx, id)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC().UnixMilli()
	var res sql.Result
	switch {
	case deliveryID != "":
		res, err = s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'pending', attempts = 0, next_ms = ? WHERE id = ? AND webhook_id = ?`,
			now, deliveryID, id)
	case !since.IsZero():
		res, err = s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'pending', attempts = 0, next_ms = ? WHERE webhook_id = ? AND state = 'failed' AND created_ms >= ?`,
			now, id, since.UnixMilli())
	default:
		return 0, fmt.Errorf("%w: name a delivery_id, or since for every failed delivery since then", ErrInvalidInput)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if deliveryID != "" && n == 0 {
		return 0, fmt.Errorf("%w: delivery %s", ErrNotFound, deliveryID)
	}
	s.record(ctx, w, "webhook.redeliver", via, fmt.Sprintf("%d deliveries", n))
	s.emitter.wakeIfAny()
	return n, nil
}

// List returns the subscriptions the caller may manage.
func (s *Service) List(ctx context.Context) ([]Webhook, error) {
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	all, err := s.load(ctx, "")
	if err != nil {
		return nil, err
	}
	out := []Webhook{}
	for _, w := range all {
		if mayManage(ctx, w.Workspaces, w.Groups) == nil {
			out = append(out, w)
		}
	}
	return out, nil
}

// Get returns one subscription the caller may manage.
func (s *Service) Get(ctx context.Context, id string) (*Webhook, error) {
	return s.manageable(ctx, id)
}

// Deliveries lists a subscription's deliveries, newest first, with their
// attempts.
func (s *Service) Deliveries(ctx context.Context, id, state string, limit int) ([]Delivery, error) {
	if _, err := s.manageable(ctx, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// LEFT JOIN: a delivery outlives its event when the event is pruned.
	q := `SELECT d.id, COALESCE(e.id, ''), COALESCE(e.type, ''), d.event_seq, d.item_id, d.state, d.attempts, d.next_ms, d.last_code, d.last_error, d.last_ms, d.created_ms
		FROM webhook_deliveries d LEFT JOIN integration_events e ON e.seq = d.event_seq WHERE d.webhook_id = ?`
	args := []any{id}
	if state != "" {
		if state != StatePending && state != StateDelivered && state != StateFailed {
			return nil, fmt.Errorf("%w: state is pending, delivered or failed", ErrInvalidInput)
		}
		q += ` AND d.state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY d.event_seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var nextMS, created int64
		var lastMS sql.NullInt64
		if err := rows.Scan(&d.ID, &d.EventID, &d.EventType, &d.EventSeq, &d.ItemID, &d.State, &d.Attempts, &nextMS, &d.LastCode, &d.LastError, &lastMS, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.CreatedAt = time.UnixMilli(created).UTC()
		if d.State == StatePending {
			t := time.UnixMilli(nextMS).UTC()
			d.NextAt = &t
		}
		if lastMS.Valid {
			t := time.UnixMilli(lastMS.Int64).UTC()
			d.LastAt = &t
		}
		out = append(out, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Log, err = s.attempts(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) attempts(ctx context.Context, deliveryID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at_ms, code, error, duration_ms FROM webhook_attempts WHERE delivery_id = ? ORDER BY at_ms`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var at int64
		if err := rows.Scan(&at, &a.Code, &a.Error, &a.DurationMS); err != nil {
			return nil, err
		}
		a.At = time.UnixMilli(at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) manageable(ctx context.Context, id string) (*Webhook, error) {
	all, err := s.load(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: webhook %s", ErrNotFound, id)
	}
	w := all[0]
	if err := mayManage(ctx, w.Workspaces, w.Groups); err != nil {
		if errors.Is(err, auth.ErrApproverRequired) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: webhook %s", ErrNotFound, id) // no oracle for foreign ones
	}
	return &w, nil
}

const webhookColumns = `id, name, url, workspaces, groups, types, secret_enc, old_secret_enc, old_until_ms, enabled, disabled_reason,
	failing_since_ms, cursor_seq, created_by, created_ms, updated_ms`

// load reads one subscription (id) or all ("").
func (s *Service) load(ctx context.Context, id string) ([]Webhook, error) {
	q := `SELECT ` + webhookColumns + ` FROM webhooks`
	var args []any
	if id != "" {
		q += ` WHERE id = ?`
		args = append(args, id)
	}
	q += ` ORDER BY created_ms`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		var ws, groups, types string
		var oldUntil, failing sql.NullInt64
		var enabled int
		var created, updated int64
		if err := rows.Scan(&w.ID, &w.Name, &w.URL, &ws, &groups, &types, &w.secret, &w.oldSecret, &oldUntil, &enabled,
			&w.DisabledReason, &failing, &w.cursor, &w.CreatedBy, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ws), &w.Workspaces)
		_ = json.Unmarshal([]byte(groups), &w.Groups)
		_ = json.Unmarshal([]byte(types), &w.Types)
		w.Enabled = enabled == 1
		if oldUntil.Valid {
			w.oldUntil = time.UnixMilli(oldUntil.Int64)
		}
		if failing.Valid {
			t := time.UnixMilli(failing.Int64).UTC()
			w.FailingSince = &t
		}
		w.CreatedAt, w.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// record writes a webhook.* audit entry under the subscription's first
// workspace, so approvers reading their workspaces see it (a group-only
// subscription's entries are root's, like the subscription).
func (s *Service) record(ctx context.Context, w *Webhook, action, via, notes string) {
	if s.audit == nil {
		return
	}
	ws := ""
	if len(w.Workspaces) > 0 {
		ws = w.Workspaces[0]
	}
	_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: ws, EntityType: "webhook", EntityID: w.ID, Action: action, Via: via, Notes: notes})
}

// sameSet: the same values, in any order (the dashboard's checklists add
// in click order).
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// groupCache holds groups read during one fan-out pass.
type groupCache map[string]*groupScope

type groupScope struct{ members, patterns []string }

func (s *Service) inGroup(ctx context.Context, cache groupCache, group, ws string) bool {
	g, ok := cache[group]
	if !ok {
		g = &groupScope{}
		if s.GroupScope != nil {
			if members, patterns, err := s.GroupScope(ctx, group); err == nil {
				g.members, g.patterns = members, patterns
			}
		}
		cache[group] = g
	}
	if slices.Contains(g.members, ws) {
		return true
	}
	for _, pat := range g.patterns {
		if ok, _ := path.Match(pat, ws); ok {
			return true
		}
	}
	return false
}

// wants reports whether the subscription takes the event: its type (the
// opt-in ones only when named) and its workspace (listed, or in a group).
func (s *Service) wants(ctx context.Context, w *Webhook, ev Stored, groups groupCache) bool {
	if ev.Type == TypeWebhookTest || ev.WorkspaceID == "" {
		return false
	}
	if len(w.Types) == 0 {
		if optIn[ev.Type] {
			return false
		}
	} else if !slices.Contains(w.Types, ev.Type) {
		return false
	}
	if slices.Contains(w.Workspaces, ev.WorkspaceID) {
		return true
	}
	for _, g := range w.Groups {
		if s.inGroup(ctx, groups, g, ev.WorkspaceID) {
			return true
		}
	}
	return false
}
