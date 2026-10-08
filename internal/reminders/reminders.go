// Package reminders stores "remind me at 3pm to fetch water" requests and works out when
// they are due. Reminders live in Postgres when available so they survive restarts and
// Render's free-tier sleep; without a database they are kept in memory.
package reminders

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the Docker image has no zoneinfo
)

type Reminder struct {
	ID        int64
	ChatID    int64
	ThreadID  int
	UserID    int64
	Username  string // who asked
	FirstName string
	Target    string // optional @username to remind instead of the asker
	ReplyTo   int    // the message that asked for it
	Text      string
	DueAt     time.Time
	Timezone  string
	Repeat    string // "", "daily", "weekly"
	DeliverBy string // "" (text), "voice_note", "telegram_call", "phone_call"
	CreatedAt time.Time
}

type Store struct {
	db *sql.DB

	mu     sync.Mutex
	mem    map[int64]*Reminder
	nextID int64
}

// New returns a store backed by db, or by memory when db is nil.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db, mem: map[int64]*Reminder{}, nextID: 1}
	if db == nil {
		return s, nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS reminders (
			id BIGSERIAL PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			thread_id INT NOT NULL DEFAULT 0,
			user_id BIGINT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			first_name TEXT NOT NULL DEFAULT '',
			target TEXT NOT NULL DEFAULT '',
			reply_to INT NOT NULL DEFAULT 0,
			text TEXT NOT NULL,
			due_at TIMESTAMPTZ NOT NULL,
			timezone TEXT NOT NULL DEFAULT '',
			repeat TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			done BOOLEAN NOT NULL DEFAULT false
		);
		CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(due_at) WHERE NOT done;
		ALTER TABLE reminders ADD COLUMN IF NOT EXISTS deliver_by TEXT NOT NULL DEFAULT '';`)
	if err != nil {
		return nil, fmt.Errorf("create reminders table: %w", err)
	}
	return s, nil
}

func (s *Store) Add(ctx context.Context, r *Reminder) error {
	r.CreatedAt = time.Now()
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		r.ID = s.nextID
		s.nextID++
		cp := *r
		s.mem[r.ID] = &cp
		return nil
	}
	return s.db.QueryRowContext(ctx, `
		INSERT INTO reminders (chat_id, thread_id, user_id, username, first_name, target, reply_to, text, due_at, timezone, repeat, deliver_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		r.ChatID, r.ThreadID, r.UserID, r.Username, r.FirstName, r.Target, r.ReplyTo, r.Text, r.DueAt, r.Timezone, r.Repeat, r.DeliverBy,
	).Scan(&r.ID)
}

const selectCols = `id, chat_id, thread_id, user_id, username, first_name, target, reply_to, text, due_at, timezone, repeat, created_at, deliver_by`

func scan(rows *sql.Rows) ([]*Reminder, error) {
	defer rows.Close()
	var out []*Reminder
	for rows.Next() {
		r := &Reminder{}
		if err := rows.Scan(&r.ID, &r.ChatID, &r.ThreadID, &r.UserID, &r.Username, &r.FirstName, &r.Target,
			&r.ReplyTo, &r.Text, &r.DueAt, &r.Timezone, &r.Repeat, &r.CreatedAt, &r.DeliverBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Due returns reminders whose time has come.
func (s *Store) Due(ctx context.Context, now time.Time) ([]*Reminder, error) {
	if s.db == nil {
		return s.memFilter(func(r *Reminder) bool { return !r.DueAt.After(now) }), nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+` FROM reminders WHERE NOT done AND due_at <= $1 ORDER BY due_at LIMIT 50`, now)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

// Pending lists a user's upcoming reminders (all users when userID is 0).
func (s *Store) Pending(ctx context.Context, userID int64) ([]*Reminder, error) {
	if s.db == nil {
		return s.memFilter(func(r *Reminder) bool { return userID == 0 || r.UserID == userID }), nil
	}
	q := `SELECT ` + selectCols + ` FROM reminders WHERE NOT done`
	args := []interface{}{}
	if userID != 0 {
		q += ` AND user_id = $1`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY due_at LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

// Complete marks a delivered reminder done, or moves a repeating one to its next time.
func (s *Store) Complete(ctx context.Context, r *Reminder, now time.Time) error {
	next, repeats := NextOccurrence(r, now)
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if repeats {
			if m, ok := s.mem[r.ID]; ok {
				m.DueAt = next
			}
		} else {
			delete(s.mem, r.ID)
		}
		return nil
	}
	if repeats {
		_, err := s.db.ExecContext(ctx, `UPDATE reminders SET due_at = $2 WHERE id = $1`, r.ID, next)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET done = true WHERE id = $1`, r.ID)
	return err
}

// Cancel removes one of a user's reminders. It reports whether anything was cancelled.
func (s *Store) Cancel(ctx context.Context, userID, id int64) (bool, error) {
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r, ok := s.mem[id]; ok && r.UserID == userID {
			delete(s.mem, id)
			return true, nil
		}
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE reminders SET done = true WHERE id = $1 AND user_id = $2 AND NOT done`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) memFilter(keep func(*Reminder) bool) []*Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Reminder
	for _, r := range s.mem {
		if keep(r) {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	return out
}

// NextOccurrence returns the next due time for a repeating reminder, skipping any
// occurrences missed while the service was asleep.
func NextOccurrence(r *Reminder, now time.Time) (time.Time, bool) {
	var step func(time.Time) time.Time
	switch r.Repeat {
	case "daily":
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	case "weekly":
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	default:
		return time.Time{}, false
	}
	loc := LoadZone(r.Timezone)
	next := step(r.DueAt.In(loc)) // stepping in the local zone keeps "3pm" at 3pm across DST
	for !next.After(now) {
		next = step(next)
	}
	return next, true
}

var zoneAliases = map[string]string{
	"lagos": "Africa/Lagos", "wat": "Africa/Lagos", "nigeria": "Africa/Lagos", "naija": "Africa/Lagos",
	"utc": "UTC", "gmt": "UTC", "london": "Europe/London", "uk": "Europe/London",
	"est": "America/New_York", "edt": "America/New_York", "new york": "America/New_York", "eastern": "America/New_York",
	"pst": "America/Los_Angeles", "pdt": "America/Los_Angeles", "pacific": "America/Los_Angeles",
	"cst": "America/Chicago", "central": "America/Chicago", "accra": "Africa/Accra", "ghana": "Africa/Accra",
	"nairobi": "Africa/Nairobi", "kenya": "Africa/Nairobi", "eat": "Africa/Nairobi",
	"johannesburg": "Africa/Johannesburg", "sast": "Africa/Johannesburg", "cet": "Europe/Berlin",
	"paris": "Europe/Paris", "berlin": "Europe/Berlin", "dubai": "Asia/Dubai", "india": "Asia/Kolkata",
	"ist": "Asia/Kolkata", "singapore": "Asia/Singapore", "tokyo": "Asia/Tokyo", "jst": "Asia/Tokyo",
}

// DefaultZone is used when nobody says which timezone they mean.
var DefaultZone = "Africa/Lagos"

// LoadZone resolves an IANA name or a common alias ("lagos time", "WAT"), falling back
// to DefaultZone.
func LoadZone(name string) *time.Location {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(strings.TrimSuffix(n, " time"), " timezone")
	if alias, ok := zoneAliases[n]; ok {
		name = alias
	}
	if name != "" {
		if loc, err := time.LoadLocation(strings.TrimSpace(name)); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation(DefaultZone); err == nil {
		return loc
	}
	return time.UTC
}

var layouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02 3:04pm",
	"2006-01-02 3pm",
}

var clockLayouts = []string{"15:04", "3:04pm", "3:04 pm", "3pm", "3 pm", "15"}

// ResolveTime turns what the model passed ("2026-10-08 15:00", "3pm", "15:30", or a
// minute offset) into an absolute time in loc. A bare clock time that has already passed
// today means tomorrow.
func ResolveTime(at string, inMinutes float64, loc *time.Location, now time.Time) (time.Time, error) {
	if inMinutes > 0 {
		return now.Add(time.Duration(inMinutes * float64(time.Minute))), nil
	}
	at = strings.ToLower(strings.TrimSpace(at))
	if at == "" {
		return time.Time{}, fmt.Errorf("no time given")
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, at, loc); err == nil {
			return t, nil
		}
	}
	local := now.In(loc)
	for _, l := range clockLayouts {
		if t, err := time.ParseInLocation(l, at, loc); err == nil {
			due := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if !due.After(now) {
				due = due.AddDate(0, 0, 1)
			}
			return due, nil
		}
	}
	if n, err := strconv.Atoi(at); err == nil && n > 0 {
		return now.Add(time.Duration(n) * time.Minute), nil
	}
	return time.Time{}, fmt.Errorf("couldn't read the time %q; use YYYY-MM-DD HH:MM", at)
}

// Describe formats a due time the way a person would say it, in the reminder's zone.
func Describe(t time.Time, loc *time.Location, now time.Time) string {
	lt, ln := t.In(loc), now.In(loc)
	clock := strings.ToLower(lt.Format("3:04pm"))
	clock = strings.Replace(clock, ":00", "", 1)
	zone := lt.Format("MST")
	switch {
	case sameDay(lt, ln):
		return "today at " + clock + " " + zone
	case sameDay(lt, ln.AddDate(0, 0, 1)):
		return "tomorrow at " + clock + " " + zone
	case lt.Sub(ln) < 6*24*time.Hour:
		return lt.Format("Monday") + " at " + clock + " " + zone
	default:
		return lt.Format("Mon 2 Jan") + " at " + clock + " " + zone
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
