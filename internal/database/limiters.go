package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/lfsc09/claude-lens/internal/status"
)

// Limiter is a row of the limiters table: a cost cap that blocks proxied
// requests once CurrentCost reaches LimitAmount, until the next refresh.
// SessionID == "" scopes it to every session (global); any other value only
// gates requests carrying that sanitized session id. ActiveStartHour and
// ActiveEndHour are either both nil (always active) or both set (0-23,
// inclusive, server-local), wrapping past midnight when start > end.
type Limiter struct {
	ID              int64   `json:"id"`
	SessionID       string  `json:"session_id"`
	LimitAmount     float64 `json:"limit_amount"`
	CurrentCost     float64 `json:"current_cost"`
	RefreshValue    int     `json:"refresh_value"`
	RefreshUnit     string  `json:"refresh_unit"` // "minutes" | "hours" | "days" | "months"
	RefreshAligned  bool    `json:"refresh_aligned"`
	NextRefreshAt   float64 `json:"next_refresh_at"`
	ActiveStartHour *int    `json:"active_start_hour"`
	ActiveEndHour   *int    `json:"active_end_hour"`
	IsActive        bool    `json:"is_active"`
	CreatedAt       float64 `json:"created_at"`
	UpdatedAt       float64 `json:"updated_at"`

	// SlackWebhookURL is where this limiter's alerts (both AlertThresholdPct
	// and AlertRequestCostUSD) are posted. Required for either alert to fire
	// — there is no process-wide fallback (see DB.SetNotifications).
	SlackWebhookURL string `json:"slack_webhook_url"`
	// AlertThresholdPct, if set, fires a one-shot Slack alert (see AlertSent)
	// the first time CurrentCost reaches this percentage of LimitAmount
	// within the current refresh window. Nil disables the alert.
	AlertThresholdPct *int `json:"alert_threshold_pct"`
	// AlertSent marks that the AlertThresholdPct alert already fired for the
	// current refresh window, so accrueLimiterCost doesn't resend it on
	// every subsequent request. Reset to false whenever the window refreshes.
	AlertSent bool `json:"alert_sent"`
	// AlertRequestCostUSD, if set, fires a Slack alert every time a single
	// exchange governed by this limiter costs at least this much. Unlike
	// AlertThresholdPct this has no "already sent" gating — it's a per-
	// request spike alert, not a window state. Nil disables it.
	AlertRequestCostUSD *float64 `json:"alert_request_cost_usd"`

	// WithinActivePeriod reports whether ActiveStartHour/ActiveEndHour
	// currently covers the server's local time. Not persisted — callers that
	// return a Limiter to the client set it via WithinActivePeriodNow so the
	// UI can render "active now" using server time instead of the browser's.
	WithinActivePeriod bool `json:"within_active_period"`
}

const limiterColumns = `id, session_id, limit_amount, current_cost, refresh_value, refresh_unit, refresh_aligned,
	next_refresh_at, active_start_hour, active_end_hour, is_active, created_at, updated_at,
	slack_webhook_url, alert_threshold_pct, alert_sent, alert_request_cost_usd`

// alignedRefreshCombos are the (refresh_unit, refresh_value) pairs with an
// unambiguous calendar boundary — the only ones RefreshAligned may be set
// for. See ComputeNextRefresh for what each boundary resolves to.
var alignedRefreshCombos = map[string][]int{
	"minutes": {60},
	"hours":   {1, 24},
	"days":    {1, 7},
	"months":  {1},
}

// SupportsAlignedRefresh reports whether unit/value is one of the six
// combos with an unambiguous calendar boundary.
func SupportsAlignedRefresh(unit string, value int) bool {
	for _, v := range alignedRefreshCombos[unit] {
		if v == value {
			return true
		}
	}
	return false
}

func scanLimiter(row interface{ Scan(dest ...any) error }) (Limiter, error) {
	var l Limiter
	var aligned, active, alertSent int
	err := row.Scan(&l.ID, &l.SessionID, &l.LimitAmount, &l.CurrentCost, &l.RefreshValue, &l.RefreshUnit, &aligned,
		&l.NextRefreshAt, &l.ActiveStartHour, &l.ActiveEndHour, &active, &l.CreatedAt, &l.UpdatedAt,
		&l.SlackWebhookURL, &l.AlertThresholdPct, &alertSent, &l.AlertRequestCostUSD)
	l.RefreshAligned = aligned != 0
	l.IsActive = active != 0
	l.AlertSent = alertSent != 0
	return l, err
}

// ListLimiters returns every limiter, grouped by session (global first)
// and then by active-period start hour.
func (db *DB) ListLimiters(ctx context.Context) ([]Limiter, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+limiterColumns+` FROM limiters ORDER BY session_id, active_start_hour`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var limiters []Limiter
	for rows.Next() {
		l, err := scanLimiter(rows)
		if err != nil {
			return nil, err
		}
		limiters = append(limiters, l)
	}
	return limiters, rows.Err()
}

// GetLimiter returns the limiter with the given id, or nil if it doesn't
// exist.
func (db *DB) GetLimiter(ctx context.Context, id int64) (*Limiter, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+limiterColumns+` FROM limiters WHERE id = ?`, id)
	l, err := scanLimiter(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLimiter inserts a new limiter and returns its id.
func (db *DB) CreateLimiter(ctx context.Context, l Limiter) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO limiters (session_id, limit_amount, current_cost, refresh_value, refresh_unit, refresh_aligned,
			next_refresh_at, active_start_hour, active_end_hour, is_active, created_at, updated_at,
			slack_webhook_url, alert_threshold_pct, alert_sent, alert_request_cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.SessionID, l.LimitAmount, l.CurrentCost, l.RefreshValue, l.RefreshUnit, boolToInt(l.RefreshAligned),
		l.NextRefreshAt, l.ActiveStartHour, l.ActiveEndHour, boolToInt(l.IsActive), l.CreatedAt, l.UpdatedAt,
		l.SlackWebhookURL, l.AlertThresholdPct, boolToInt(l.AlertSent), l.AlertRequestCostUSD,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateLimiter replaces a limiter's rule fields in place. IsActive is left
// untouched — that's SetLimiterActive's job, kept separate so toggling
// never resets progress the way a full edit does.
func (db *DB) UpdateLimiter(ctx context.Context, l Limiter) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE limiters SET session_id = ?, limit_amount = ?, current_cost = ?, refresh_value = ?, refresh_unit = ?,
			refresh_aligned = ?, next_refresh_at = ?, active_start_hour = ?, active_end_hour = ?, updated_at = ?,
			slack_webhook_url = ?, alert_threshold_pct = ?, alert_sent = ?, alert_request_cost_usd = ?
		 WHERE id = ?`,
		l.SessionID, l.LimitAmount, l.CurrentCost, l.RefreshValue, l.RefreshUnit, boolToInt(l.RefreshAligned),
		l.NextRefreshAt, l.ActiveStartHour, l.ActiveEndHour, l.UpdatedAt,
		l.SlackWebhookURL, l.AlertThresholdPct, boolToInt(l.AlertSent), l.AlertRequestCostUSD, l.ID,
	)
	return err
}

// SetLimiterActive toggles a limiter's master switch without touching its
// accrued progress.
func (db *DB) SetLimiterActive(ctx context.Context, id int64, active bool, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE limiters SET is_active = ?, updated_at = ? WHERE id = ?`, boolToInt(active), updatedAt, id)
	return err
}

// DeleteLimiter removes a limiter by id. It is not an error if the id does
// not exist.
func (db *DB) DeleteLimiter(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM limiters WHERE id = ?", id)
	return err
}

// FindOverlappingLimiter returns a limiter sharing sessionID whose active
// period overlaps [startHour, endHour], or nil if none does. excludeID
// skips a row being updated (pass 0 when creating). Global (session_id ==
// "") and per-session limiters are separate groups — this only ever
// compares within the group named by sessionID.
func (db *DB) FindOverlappingLimiter(ctx context.Context, sessionID string, excludeID int64, startHour, endHour *int) (*Limiter, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+limiterColumns+` FROM limiters WHERE session_id = ? AND id != ?`,
		sessionID, excludeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mask := hourMask(startHour, endHour)
	for rows.Next() {
		l, err := scanLimiter(rows)
		if err != nil {
			return nil, err
		}
		if mask&hourMask(l.ActiveStartHour, l.ActiveEndHour) != 0 {
			return &l, nil
		}
	}
	return nil, rows.Err()
}

// hourMask returns the bitmask of hours (bit i set means hour i is
// covered) for a start/end pair. Both nil means every hour. Handles the
// overnight wraparound case (start > end) and the single-hour case
// (start == end).
func hourMask(startHour, endHour *int) uint32 {
	if startHour == nil || endHour == nil {
		return 0xFFFFFF
	}
	start, end := *startHour, *endHour
	var mask uint32
	if start <= end {
		for h := start; h <= end; h++ {
			mask |= 1 << uint(h)
		}
	} else {
		for h := start; h <= 23; h++ {
			mask |= 1 << uint(h)
		}
		for h := 0; h <= end; h++ {
			mask |= 1 << uint(h)
		}
	}
	return mask
}

// withinActivePeriod reports whether now falls inside l's active-period
// hour range (server-local time). Both bounds nil means always active;
// otherwise the range is inclusive on both ends and wraps past midnight
// when start > end.
func withinActivePeriod(l Limiter, now time.Time) bool {
	if l.ActiveStartHour == nil || l.ActiveEndHour == nil {
		return true
	}
	hour := now.Hour()
	start, end := *l.ActiveStartHour, *l.ActiveEndHour
	if start <= end {
		return hour >= start && hour <= end
	}
	return hour >= start || hour <= end
}

// WithinActivePeriodNow reports whether l's active-period hour range covers
// the current server-local time. Exported for API responses (see
// Limiter.WithinActivePeriod) so the client renders "active now" using
// server time rather than replicating this check in the browser's own
// timezone.
func WithinActivePeriodNow(l Limiter) bool {
	return withinActivePeriod(l, time.Now())
}

// ComputeNextRefresh returns the next refresh boundary for a limiter's
// refresh settings, strictly after now. Aligned mode is only meaningful for
// the six combos in alignedRefreshCombos (validated at the API layer); any
// other (unit, value, aligned=true) combination falls back to a fixed
// interval here rather than guessing at a boundary.
func ComputeNextRefresh(now time.Time, unit string, value int, aligned bool) time.Time {
	if aligned {
		switch {
		case unit == "minutes" && value == 60, unit == "hours" && value == 1:
			return now.Truncate(time.Hour).Add(time.Hour)
		case unit == "hours" && value == 24, unit == "days" && value == 1:
			y, m, d := now.Date()
			return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
		case unit == "days" && value == 7:
			y, m, d := now.Date()
			midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
			daysUntilMonday := (int(time.Monday-midnight.Weekday()) + 7) % 7
			if daysUntilMonday == 0 {
				daysUntilMonday = 7
			}
			return midnight.AddDate(0, 0, daysUntilMonday)
		case unit == "months" && value == 1:
			y, m, _ := now.Date()
			return time.Date(y, m+1, 1, 0, 0, 0, 0, now.Location())
		}
	}
	switch unit {
	case "minutes":
		return now.Add(time.Duration(value) * time.Minute)
	case "hours":
		return now.Add(time.Duration(value) * time.Hour)
	case "days":
		return now.Add(time.Duration(value) * 24 * time.Hour)
	case "months":
		return now.AddDate(0, value, 0)
	default:
		return now.Add(time.Duration(value) * time.Minute)
	}
}

// refreshIfDue zeroes CurrentCost, resets AlertSent, and recomputes
// NextRefreshAt when now has reached it, returning whether it fired. The
// caller is responsible for persisting the change.
func refreshIfDue(l *Limiter, now time.Time) bool {
	if float64(now.Unix()) < l.NextRefreshAt {
		return false
	}
	l.CurrentCost = 0
	l.AlertSent = false
	l.NextRefreshAt = float64(ComputeNextRefresh(now, l.RefreshUnit, l.RefreshValue, l.RefreshAligned).Unix())
	return true
}

// persistLimiterProgress writes back a limiter's current_cost,
// next_refresh_at, and alert_sent after refreshIfDue and/or an accrual.
func (db *DB) persistLimiterProgress(ctx context.Context, l Limiter, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE limiters SET current_cost = ?, next_refresh_at = ?, alert_sent = ?, updated_at = ? WHERE id = ?`,
		l.CurrentCost, l.NextRefreshAt, boolToInt(l.AlertSent), updatedAt, l.ID,
	)
	return err
}

// applicableLimiters returns every limiter that governs sessionID: the
// global limiter(s) (session_id == "") plus any scoped to sessionID itself.
func (db *DB) applicableLimiters(ctx context.Context, sessionID string) ([]Limiter, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+limiterColumns+` FROM limiters WHERE session_id = ? OR session_id = ''`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var limiters []Limiter
	for rows.Next() {
		l, err := scanLimiter(rows)
		if err != nil {
			return nil, err
		}
		limiters = append(limiters, l)
	}
	return limiters, rows.Err()
}

// RefreshDueLimiters resets every limiter whose next refresh boundary has
// already passed, independent of any proxied request reaching it. Mirrors
// the lazy refresh applied in CheckLimiters/accrueLimiterCost — including
// running regardless of IsActive, so a paused limiter still gets a clean
// window whenever it's re-enabled — but is driven by RunLimiterRefreshLoop
// instead of request traffic, so an idle limiter recovers on schedule even
// with nothing hitting the proxy. Returns how many limiters were refreshed.
func (db *DB) RefreshDueLimiters(ctx context.Context) (int, error) {
	now := time.Now()
	rows, err := db.sql.QueryContext(ctx, `SELECT `+limiterColumns+` FROM limiters WHERE next_refresh_at <= ?`, float64(now.Unix()))
	if err != nil {
		return 0, err
	}
	var due []Limiter
	for rows.Next() {
		l, err := scanLimiter(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, l)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	for i := range due {
		l := &due[i]
		if refreshIfDue(l, now) {
			if err := db.persistLimiterProgress(ctx, *l, float64(now.Unix())); err != nil {
				return i, fmt.Errorf("refresh limiter %d: %w", l.ID, err)
			}
		}
	}
	return len(due), nil
}

// timeUntilNextMinute returns the duration until the next minute boundary
// strictly after now.
func timeUntilNextMinute(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}

// RunLimiterRefreshLoop refreshes due limiters at the start of every minute
// until ctx is done, so a limiter recovers on schedule even when no proxied
// request arrives to trigger the lazy refresh in CheckLimiters/
// accrueLimiterCost. fresh is bumped whenever a limiter actually changes, so
// the admin SSE stream can push clients a refetch signal even though no
// exchange was saved. Meant to be run in its own goroutine for the lifetime
// of the process.
func (db *DB) RunLimiterRefreshLoop(ctx context.Context, fresh *status.Fresh) {
	logger := slog.Default().With("component", "database")

	timer := time.NewTimer(timeUntilNextMinute(time.Now()))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if n, err := db.RefreshDueLimiters(ctx); err != nil {
				logger.Error("refresh due limiters", "error", err)
			} else if n > 0 {
				logger.Info("refreshed due limiters", "count", n)
				fresh.Bump()
			}
			timer.Reset(timeUntilNextMinute(time.Now()))
		}
	}
}

// refreshApplicableLimiters returns every limiter governing sessionID (see
// applicableLimiters), lazily applying and persisting any refresh boundary
// already due. Shared by CheckLimiters and accrueLimiterCost so a due
// refresh is applied identically regardless of which one observes it first.
func (db *DB) refreshApplicableLimiters(ctx context.Context, sessionID string) ([]Limiter, error) {
	limiters, err := db.applicableLimiters(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	for i := range limiters {
		l := &limiters[i]
		if refreshIfDue(l, now) {
			if err := db.persistLimiterProgress(ctx, *l, float64(now.Unix())); err != nil {
				return nil, fmt.Errorf("refresh limiter %d: %w", l.ID, err)
			}
		}
	}
	return limiters, nil
}

// CheckLimiters reports whether a request for sessionID should be blocked:
// true if any applicable, active, currently-in-window limiter has already
// reached its LimitAmount. Refresh boundaries due by now are applied (and
// persisted) as a side effect, so a limiter that was due to reset gets a
// fresh window before being judged.
func (db *DB) CheckLimiters(ctx context.Context, sessionID string) (bool, *Limiter, error) {
	limiters, err := db.refreshApplicableLimiters(ctx, sessionID)
	if err != nil {
		return false, nil, err
	}

	now := time.Now()
	for i := range limiters {
		l := &limiters[i]
		if l.IsActive && withinActivePeriod(*l, now) && l.CurrentCost >= l.LimitAmount {
			blocked := *l
			return true, &blocked, nil
		}
	}
	return false, nil, nil
}

// accrueLimiterCost adds cost to every applicable, active, currently-in-
// window limiter's running total, and fires whichever Slack alerts (best-
// effort, asynchronous — see sendBudgetAlert/sendRequestSpikeAlert) this
// accrual newly triggers: a budget-threshold alert the moment
// AlertThresholdPct is crossed, and a request-spike alert whenever this
// single exchange's cost meets AlertRequestCostUSD. A limiter that is
// inactive or outside its active period right now simply doesn't track this
// request, or alert on it, at all.
//
// Locked on alertMu for its whole body: two exchanges completing close
// together for the same limiter must not both observe AlertSent == false
// and send a duplicate budget alert.
func (db *DB) accrueLimiterCost(ctx context.Context, sessionID string, cost float64, model *string) error {
	db.alertMu.Lock()
	defer db.alertMu.Unlock()

	limiters, err := db.refreshApplicableLimiters(ctx, sessionID)
	if err != nil {
		return err
	}

	now := time.Now()
	for i := range limiters {
		l := &limiters[i]
		if !l.IsActive || !withinActivePeriod(*l, now) {
			continue
		}

		previousCost := l.CurrentCost
		l.CurrentCost += cost

		if l.AlertThresholdPct != nil && !l.AlertSent && l.LimitAmount > 0 {
			threshold := l.LimitAmount * float64(*l.AlertThresholdPct) / 100
			if l.CurrentCost >= threshold && previousCost < threshold {
				l.AlertSent = true
				db.sendBudgetAlert(*l, *l.AlertThresholdPct)
			}
		}

		if l.AlertRequestCostUSD != nil && cost >= *l.AlertRequestCostUSD {
			db.sendRequestSpikeAlert(*l, cost, model)
		}

		if err := db.persistLimiterProgress(ctx, *l, float64(now.Unix())); err != nil {
			return fmt.Errorf("accrue limiter %d: %w", l.ID, err)
		}
	}
	return nil
}

// sendBudgetAlert dispatches a Slack notification for l having crossed pct
// of its budget, to l's own SlackWebhookURL. Fire-and-forget in its own
// goroutine with a background context: delivery failures are logged, never
// propagated, so a slow or unreachable Slack endpoint can't delay the
// proxied request or the exchange save that triggered this accrual.
func (db *DB) sendBudgetAlert(l Limiter, pct int) {
	db.sendAlert(l.SlackWebhookURL, budgetAlertText(l, pct), l)
}

// sendRequestSpikeAlert dispatches a Slack notification for a single
// exchange, governed by l, whose cost met l's AlertRequestCostUSD. Same
// fire-and-forget delivery as sendBudgetAlert.
func (db *DB) sendRequestSpikeAlert(l Limiter, cost float64, model *string) {
	db.sendAlert(l.SlackWebhookURL, requestSpikeAlertText(l, cost, model), l)
}

// sendAlert posts text to webhookURL in its own goroutine, logging (never
// propagating) delivery failures. A no-op when notifications aren't wired up
// or the limiter has no webhook configured.
func (db *DB) sendAlert(webhookURL, text string, l Limiter) {
	if db.notifier == nil || webhookURL == "" {
		return
	}
	go func() {
		if err := db.notifier.Send(context.Background(), webhookURL, text); err != nil {
			slog.Error("slack alert failed", "error", err, "limiter_id", l.ID, "session_id", l.SessionID)
		}
	}()
}

// budgetAlertText formats the Slack message body for a limiter crossing its
// alert threshold.
func budgetAlertText(l Limiter, pct int) string {
	return fmt.Sprintf(":warning: claude-lens: %s limiter has spent $%.2f of its $%.2f budget (%d%% threshold reached)",
		limiterScope(l), l.CurrentCost, l.LimitAmount, pct)
}

// requestSpikeAlertText formats the Slack message body for a single exchange
// costing at least l's AlertRequestCostUSD.
func requestSpikeAlertText(l Limiter, cost float64, model *string) string {
	m := "unknown model"
	if model != nil {
		m = *model
	}
	return fmt.Sprintf(":rotating_light: claude-lens: a single request cost $%.2f (%s, %s) — over the $%.2f alert threshold",
		cost, m, limiterScope(l), *l.AlertRequestCostUSD)
}

// limiterScope renders l's session_id as "global" or "session <id>" for
// Slack alert text.
func limiterScope(l Limiter) string {
	if l.SessionID == "" {
		return "global"
	}
	return "session " + l.SessionID
}
