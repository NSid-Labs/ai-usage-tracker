package push

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultOpenCodeDB returns OpenCode's database path ($XDG_DATA_HOME/opencode/opencode.db).
func DefaultOpenCodeDB() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "opencode", "opencode.db")
}

// Queries for the two storage layouts OpenCode has used. Newer versions keep
// assistant turns in session_message (and migrated older history into it);
// earlier SQLite versions used the message table. Both return:
// time_created (ms), provider, model, cost, input, output, reasoning, cache read, cache write.
const (
	openCodeV2Query = `SELECT time_created,
		COALESCE(json_extract(data, '$.model.providerID'), ''), COALESCE(json_extract(data, '$.model.id'), ''),
		COALESCE(json_extract(data, '$.cost'), 0),
		COALESCE(json_extract(data, '$.tokens.input'), 0), COALESCE(json_extract(data, '$.tokens.output'), 0),
		COALESCE(json_extract(data, '$.tokens.reasoning'), 0),
		COALESCE(json_extract(data, '$.tokens.cache.read'), 0), COALESCE(json_extract(data, '$.tokens.cache.write'), 0)
		FROM session_message
		WHERE type = 'assistant' AND time_created >= ? AND json_extract(data, '$.tokens') IS NOT NULL`
	openCodeV1Query = `SELECT time_created,
		COALESCE(json_extract(data, '$.providerID'), ''), COALESCE(json_extract(data, '$.modelID'), ''),
		COALESCE(json_extract(data, '$.cost'), 0),
		COALESCE(json_extract(data, '$.tokens.input'), 0), COALESCE(json_extract(data, '$.tokens.output'), 0),
		COALESCE(json_extract(data, '$.tokens.reasoning'), 0),
		COALESCE(json_extract(data, '$.tokens.cache.read'), 0), COALESCE(json_extract(data, '$.tokens.cache.write'), 0)
		FROM message
		WHERE json_extract(data, '$.role') = 'assistant' AND time_created >= ? AND json_extract(data, '$.tokens') IS NOT NULL`
)

// ReadOpenCode aggregates the last `days` UTC days of OpenCode usage per day and
// provider/model. The database is opened read-only.
func ReadOpenCode(ctx context.Context, path string, days int) ([]Row, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("opencode database: %w", err)
	}
	// A file: URL needs an absolute path with forward slashes. On Windows the
	// drive letter must be preceded by a slash ("file:///C:/...") or the driver
	// reads "C:" as a URI authority.
	p := filepath.ToSlash(path)
	if p[0] != '/' {
		p = "/" + p
	}
	dsn := (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := openCodeV1Query
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_message'`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		query = openCodeV2Query
	}

	since := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := db.QueryContext(ctx, query, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agg := map[[2]string]*Row{}
	for rows.Next() {
		var (
			ms                                      int64
			provider, model                         string
			cost                                    float64
			input, output, reasoning, cRead, cWrite int64
		)
		if err := rows.Scan(&ms, &provider, &model, &cost, &input, &output, &reasoning, &cRead, &cWrite); err != nil {
			return nil, err
		}
		day := time.UnixMilli(ms).UTC().Format(time.DateOnly)
		name := model
		if provider != "" {
			name = provider + "/" + model
		}
		if name == "" {
			name = "unknown"
		}
		k := [2]string{day, name}
		r := agg[k]
		if r == nil {
			r = &Row{Day: day, Model: name}
			agg[k] = r
		}
		r.Input += input
		r.Output += output + reasoning
		r.CacheRead += cRead
		r.CacheWrite += cWrite
		r.Requests++
		r.CostUSD += cost
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Row, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}
