package calls

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Phonebook keeps the numbers people chose to give Jasmine so she can phone them.
// Numbers stay in Postgres (never in prompts or chat replies).
type Phonebook struct {
	db  *sql.DB
	mu  sync.Mutex
	mem map[int64]string
}

func NewPhonebook(ctx context.Context, db *sql.DB) (*Phonebook, error) {
	p := &Phonebook{db: db, mem: map[int64]string{}}
	if db == nil {
		return p, nil
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS user_phones (
		user_id BIGINT PRIMARY KEY,
		username TEXT NOT NULL DEFAULT '',
		phone TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return nil, fmt.Errorf("create user_phones: %w", err)
	}
	return p, nil
}

func (p *Phonebook) Set(ctx context.Context, userID int64, username, phone string) error {
	if p.db == nil {
		p.mu.Lock()
		p.mem[userID] = phone
		p.mu.Unlock()
		return nil
	}
	_, err := p.db.ExecContext(ctx, `INSERT INTO user_phones (user_id, username, phone) VALUES ($1,$2,$3)
		ON CONFLICT (user_id) DO UPDATE SET username = EXCLUDED.username, phone = EXCLUDED.phone, updated_at = NOW()`,
		userID, strings.ToLower(username), phone)
	return err
}

func (p *Phonebook) Get(ctx context.Context, userID int64) (string, error) {
	if p.db == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.mem[userID], nil
	}
	var phone string
	err := p.db.QueryRowContext(ctx, `SELECT phone FROM user_phones WHERE user_id = $1`, userID).Scan(&phone)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return phone, err
}

// LookupByUsername finds someone else's number (owners only, enforced by the caller).
func (p *Phonebook) LookupByUsername(ctx context.Context, username string) (int64, string, error) {
	username = strings.ToLower(strings.TrimPrefix(username, "@"))
	if p.db == nil {
		return 0, "", nil
	}
	var id int64
	var phone string
	err := p.db.QueryRowContext(ctx, `SELECT user_id, phone FROM user_phones WHERE username = $1`, username).Scan(&id, &phone)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return id, phone, err
}

func (p *Phonebook) Delete(ctx context.Context, userID int64) error {
	if p.db == nil {
		p.mu.Lock()
		delete(p.mem, userID)
		p.mu.Unlock()
		return nil
	}
	_, err := p.db.ExecContext(ctx, `DELETE FROM user_phones WHERE user_id = $1`, userID)
	return err
}

var nonDigits = regexp.MustCompile(`[^\d+]`)

// NormalizePhone turns "0803 123 4567" or "+234 803-123-4567" into E.164. Local numbers
// starting with 0 are read with defaultCountry (e.g. "234" for Nigeria).
func NormalizePhone(raw, defaultCountry string) (string, error) {
	n := nonDigits.ReplaceAllString(strings.TrimSpace(raw), "")
	switch {
	case strings.HasPrefix(n, "+"):
	case strings.HasPrefix(n, "00"):
		n = "+" + n[2:]
	case strings.HasPrefix(n, "0") && defaultCountry != "":
		n = "+" + defaultCountry + n[1:]
	case defaultCountry != "" && strings.HasPrefix(n, defaultCountry):
		n = "+" + n
	default:
		n = "+" + n
	}
	digits := strings.TrimPrefix(n, "+")
	if strings.Contains(digits, "+") || len(digits) < 8 || len(digits) > 15 {
		return "", fmt.Errorf("%q doesn't look like a phone number", raw)
	}
	return n, nil
}

// MaskPhone shows only the last digits, for confirmations.
func MaskPhone(p string) string {
	if len(p) <= 4 {
		return p
	}
	return strings.Repeat("•", 3) + p[len(p)-4:]
}
