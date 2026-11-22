// Package store persists accounts and usage data in SQLite.
package store

import (
	"database/sql"
	"sort"
	"sync/atomic"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	provider   TEXT    NOT NULL,
	secret     BLOB    NOT NULL,
	plan       TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	last_poll  INTEGER NOT NULL DEFAULT 0,
	last_error TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS daily_usage (
	account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	day         TEXT    NOT NULL,
	model       TEXT    NOT NULL,
	input       INTEGER NOT NULL DEFAULT 0,
	output      INTEGER NOT NULL DEFAULT 0,
	cache_read  INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	requests    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, day, model)
);
CREATE TABLE IF NOT EXISTS daily_cost (
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	day        TEXT    NOT NULL,
	usd        REAL    NOT NULL,
	PRIMARY KEY (account_id, day)
);
CREATE TABLE IF NOT EXISTS quota_snapshots (
	account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	ts           INTEGER NOT NULL,
	win          TEXT    NOT NULL,
	used_percent REAL    NOT NULL,
	resets_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS quota_snapshots_idx ON quota_snapshots(account_id, win, ts);
`

type Store struct {
	db            *sql.DB
	retentionDays atomic.Int64 // how long quota history is kept
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + authSchema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	set, err := s.GetSettings()
	if err != nil {
		db.Close()
		return nil, err
	}
	s.retentionDays.Store(int64(set.RetentionDays))
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate adds columns introduced after the first release.
func (s *Store) migrate() error {
	for _, m := range []struct{ table, column, ddl string }{
		{"accounts", "balance", `ALTER TABLE accounts ADD COLUMN balance REAL`},
		{"quota_snapshots", "detail", `ALTER TABLE quota_snapshots ADD COLUMN detail TEXT NOT NULL DEFAULT ''`},
	} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec(m.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

type Account struct {
	ID        int64
	Name      string
	Provider  string
	Secret    []byte
	Plan      string
	CreatedAt int64
	LastPoll  int64
	LastError string
	Balance   *float64 // remaining prepaid credit in USD, nil if unknown
}

const accountCols = `id, name, provider, secret, plan, created_at, last_poll, last_error, balance`

func scanAccount(r interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := r.Scan(&a.ID, &a.Name, &a.Provider, &a.Secret, &a.Plan, &a.CreatedAt, &a.LastPoll, &a.LastError, &a.Balance)
	return a, err
}

func (s *Store) CreateAccount(name, provider string, secret []byte) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO accounts(name, provider, secret, created_at) VALUES (?, ?, ?, ?)`,
		name, provider, secret, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(id int64) (Account, error) {
	return scanAccount(s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

func (s *Store) RenameAccount(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE accounts SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Store) UpdateSecret(id int64, secret []byte) error {
	_, err := s.db.Exec(`UPDATE accounts SET secret = ? WHERE id = ?`, secret, id)
	return err
}

func (s *Store) RecordError(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE accounts SET last_poll = ?, last_error = ? WHERE id = ?`, time.Now().Unix(), msg, id)
	return err
}

// SaveResult stores one successful poll.
func (s *Store) SaveResult(id int64, r *core.Result) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	for _, u := range r.Usage {
		if _, err := tx.Exec(`INSERT INTO daily_usage(account_id, day, model, input, output, cache_read, cache_write, requests)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id, day, model) DO UPDATE SET input=excluded.input, output=excluded.output,
				cache_read=excluded.cache_read, cache_write=excluded.cache_write, requests=excluded.requests`,
			id, u.Day, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Requests); err != nil {
			return err
		}
	}
	for _, c := range r.Costs {
		if _, err := tx.Exec(`INSERT INTO daily_cost(account_id, day, usd) VALUES (?, ?, ?)
			ON CONFLICT(account_id, day) DO UPDATE SET usd=excluded.usd`, id, c.Day, c.USD); err != nil {
			return err
		}
	}
	for _, q := range r.Quotas {
		if _, err := tx.Exec(`INSERT INTO quota_snapshots(account_id, ts, win, used_percent, resets_at, detail) VALUES (?, ?, ?, ?, ?, ?)`,
			id, now, q.Window, q.UsedPercent, q.ResetsAt, q.Detail); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM quota_snapshots WHERE account_id = ? AND ts < ?`, id, now-s.retentionDays.Load()*86400); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_poll = ?, last_error = '',
		plan = CASE WHEN ? != '' THEN ? ELSE plan END, balance = COALESCE(?, balance) WHERE id = ?`,
		now, r.Plan, r.Plan, r.Balance, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LatestQuotas(id int64) ([]core.Quota, error) {
	// One row per window from the latest poll; MAX(rowid) makes SQLite take the
	// bare columns from the newest row if two polls landed in the same second.
	rows, err := s.db.Query(`SELECT win, used_percent, resets_at, detail, MAX(rowid) FROM quota_snapshots
		WHERE account_id = ? AND ts = (SELECT MAX(ts) FROM quota_snapshots WHERE account_id = ?)
		GROUP BY win ORDER BY win`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Quota
	for rows.Next() {
		var q core.Quota
		var rowid int64
		if err := rows.Scan(&q.Window, &q.UsedPercent, &q.ResetsAt, &q.Detail, &rowid); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// QuotaSample is one recorded reading of a limit window.
type QuotaSample struct {
	TS          int64
	UsedPercent float64
	ResetsAt    int64
}

// QuotaHistory returns samples for one window since the given time, oldest first.
func (s *Store) QuotaHistory(id int64, window string, since time.Time) ([]QuotaSample, error) {
	rows, err := s.db.Query(`SELECT ts, used_percent, resets_at FROM quota_snapshots WHERE account_id = ? AND win = ? AND ts >= ? ORDER BY ts`,
		id, window, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaSample
	for rows.Next() {
		var q QuotaSample
		if err := rows.Scan(&q.TS, &q.UsedPercent, &q.ResetsAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

type DayTotal struct {
	Day                                            string
	USD                                            float64
	Input, Output, CacheRead, CacheWrite, Requests int64
}

func (d DayTotal) Tokens() int64 { return d.Input + d.Output + d.CacheRead + d.CacheWrite }

// DailyTotals returns one row per day for the last n days (oldest first), zero-filled.
func (s *Store) DailyTotals(id int64, days int) ([]DayTotal, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(days - 1))
	byDay := make(map[string]*DayTotal, days)
	out := make([]DayTotal, days)
	for i := range out {
		out[i].Day = start.AddDate(0, 0, i).Format(time.DateOnly)
		byDay[out[i].Day] = &out[i]
	}
	since := start.Format(time.DateOnly)

	rows, err := s.db.Query(`SELECT day, SUM(input), SUM(output), SUM(cache_read), SUM(cache_write), SUM(requests)
		FROM daily_usage WHERE account_id = ? AND day >= ? GROUP BY day`, id, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d DayTotal
		if err := rows.Scan(&d.Day, &d.Input, &d.Output, &d.CacheRead, &d.CacheWrite, &d.Requests); err != nil {
			rows.Close()
			return nil, err
		}
		if t := byDay[d.Day]; t != nil {
			t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Requests = d.Input, d.Output, d.CacheRead, d.CacheWrite, d.Requests
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT day, usd FROM daily_cost WHERE account_id = ? AND day >= ?`, id, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var usd float64
		if err := rows.Scan(&day, &usd); err != nil {
			return nil, err
		}
		if t := byDay[day]; t != nil {
			t.USD = usd
		}
	}
	return out, rows.Err()
}

// AccountDayTotal is one account's usage on one UTC day. It is the building
// block of the History page, which mixes accounts, days and models.
type AccountDayTotal struct {
	AccountID                                      int64
	Day                                            string
	USD                                            float64
	Input, Output, CacheRead, CacheWrite, Requests int64
}

func (d AccountDayTotal) Tokens() int64 { return d.Input + d.Output + d.CacheRead + d.CacheWrite }

// DailyUsageSince returns per-account daily totals since the given day
// (inclusive), oldest first. accountID 0 means every account.
func (s *Store) DailyUsageSince(since string, accountID int64) ([]AccountDayTotal, error) {
	filter := ""
	args := []any{since}
	if accountID > 0 {
		filter = " AND account_id = ?"
		args = append(args, accountID)
	}
	type key struct {
		id  int64
		day string
	}
	byKey := map[key]*AccountDayTotal{}

	rows, err := s.db.Query(`SELECT account_id, day, SUM(input), SUM(output), SUM(cache_read), SUM(cache_write), SUM(requests)
		FROM daily_usage WHERE day >= ?`+filter+` GROUP BY account_id, day`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d AccountDayTotal
		if err := rows.Scan(&d.AccountID, &d.Day, &d.Input, &d.Output, &d.CacheRead, &d.CacheWrite, &d.Requests); err != nil {
			rows.Close()
			return nil, err
		}
		byKey[key{d.AccountID, d.Day}] = &d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT account_id, day, usd FROM daily_cost WHERE day >= ?`+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var day string
		var usd float64
		if err := rows.Scan(&id, &day, &usd); err != nil {
			return nil, err
		}
		k := key{id, day}
		if d := byKey[k]; d != nil {
			d.USD = usd
		} else {
			byKey[k] = &AccountDayTotal{AccountID: id, Day: day, USD: usd}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]AccountDayTotal, 0, len(byKey))
	for _, d := range byKey {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out, nil
}

type ModelTotal struct {
	Model                                          string
	Input, Output, CacheRead, CacheWrite, Requests int64
}

func (m ModelTotal) Tokens() int64 { return m.Input + m.Output + m.CacheRead + m.CacheWrite }

func (s *Store) ModelTotals(id int64, days int) ([]ModelTotal, error) {
	since := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format(time.DateOnly)
	return s.ModelTotalsSince(since, id)
}

// ModelTotalsSince returns per-model totals since the given day, for one
// account or for every account when accountID is 0.
func (s *Store) ModelTotalsSince(since string, accountID int64) ([]ModelTotal, error) {
	filter := ""
	args := []any{since}
	if accountID > 0 {
		filter = " AND account_id = ?"
		args = append(args, accountID)
	}
	rows, err := s.db.Query(`SELECT model, SUM(input), SUM(output), SUM(cache_read), SUM(cache_write), SUM(requests)
		FROM daily_usage WHERE day >= ?`+filter+` GROUP BY model
		ORDER BY SUM(input + output + cache_read + cache_write) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelTotal
	for rows.Next() {
		var m ModelTotal
		if err := rows.Scan(&m.Model, &m.Input, &m.Output, &m.CacheRead, &m.CacheWrite, &m.Requests); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
