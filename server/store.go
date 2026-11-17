package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sudiptadeb/synckosh"

	_ "modernc.org/sqlite"
)

// store keeps accounts, device tokens and the newest item for each type and guid, in one
// SQLite file.
type store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id      TEXT PRIMARY KEY,
  created INTEGER NOT NULL,
  seq     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS devices (
  id      TEXT PRIMARY KEY,
  account TEXT NOT NULL REFERENCES accounts(id),
  name    TEXT NOT NULL,
  token   BLOB NOT NULL UNIQUE,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
  account TEXT NOT NULL,
  type    TEXT NOT NULL,
  guid    TEXT NOT NULL,
  ms      INTEGER NOT NULL,
  n       INTEGER NOT NULL,
  dev     TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  body    BLOB NOT NULL,
  PRIMARY KEY (account, type, guid)
);
CREATE INDEX IF NOT EXISTS items_by_seq ON items (account, seq);
`

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite takes one writer at a time anyway, and every query here is short.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

func newID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

type device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

// signup makes an account with its first device.
func (s *store) signup(ctx context.Context, name string) (account, dev, token string, err error) {
	account, dev, token = newID("a_", 12), newID("d_", 12), newID("pbs_", 32)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts (id, created) VALUES (?, ?)`, account, now); err != nil {
		return "", "", "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices (id, account, name, token, created) VALUES (?, ?, ?, ?, ?)`,
		dev, account, name, tokenHash(token), now); err != nil {
		return "", "", "", err
	}
	return account, dev, token, tx.Commit()
}

func (s *store) addDevice(ctx context.Context, account, name string) (dev, token string, err error) {
	dev, token = newID("d_", 12), newID("pbs_", 32)
	_, err = s.db.ExecContext(ctx, `INSERT INTO devices (id, account, name, token, created) VALUES (?, ?, ?, ?, ?)`,
		dev, account, name, tokenHash(token), time.Now().Unix())
	return dev, token, err
}

var errNoDevice = errors.New("no such device")

func (s *store) removeDevice(ctx context.Context, account, dev string) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ? AND account = ?`, dev, account)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errNoDevice
	}
	return nil
}

func (s *store) devices(ctx context.Context, account string) ([]device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created FROM devices WHERE account = ? ORDER BY created, id`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []device{}
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.ID, &d.Name, &d.Created); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// auth returns the account and device a token belongs to.
func (s *store) auth(ctx context.Context, token string) (account, dev string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT account, id FROM devices WHERE token = ?`, tokenHash(token)).Scan(&account, &dev)
	return account, dev, err
}

type result struct {
	OK      bool            `json:"ok"`
	Seq     int64           `json:"seq,omitempty"`
	Current json.RawMessage `json:"current,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// incoming is one pushed item, checked: its envelope read, and its body as it will be stored.
type incoming struct {
	it   synckosh.Item
	body []byte
	err  string
}

// put stores each item made on the item the server holds, the one its base names. Otherwise
// it returns the item the server holds, for the device to merge with. Pushing the same edit
// again is a success, so a device can retry a push whose answer it never got.
func (s *store) put(ctx context.Context, account string, items []incoming) ([]result, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM accounts WHERE id = ?`, account).Scan(&seq); err != nil {
		return nil, false, err
	}
	start := seq
	out := make([]result, len(items))
	for i, in := range items {
		if in.err != "" {
			out[i] = result{Error: in.err}
			continue
		}
		var cur synckosh.TS
		var curSeq int64
		var curBody []byte
		err := tx.QueryRowContext(ctx, `SELECT ms, n, dev, seq, body FROM items WHERE account = ? AND type = ? AND guid = ?`,
			account, in.it.Type, in.it.GUID).Scan(&cur.MS, &cur.N, &cur.Dev, &curSeq, &curBody)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, false, err
		case in.it.TS == cur:
			// The device's timestamp names one edit: this is a retry.
			out[i] = result{OK: true, Seq: curSeq}
			continue
		case in.it.Base != curSeq:
			// The device made this edit without seeing the current item. It gets the
			// current one back, merges, and pushes again.
			c, err := withSeq(curBody, curSeq)
			if err != nil {
				return nil, false, err
			}
			out[i] = result{Current: c}
			continue
		}
		seq++
		if _, err := tx.ExecContext(ctx, `INSERT INTO items (account, type, guid, ms, n, dev, seq, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (account, type, guid) DO UPDATE SET ms = excluded.ms, n = excluded.n, dev = excluded.dev, seq = excluded.seq, body = excluded.body`,
			account, in.it.Type, in.it.GUID, in.it.TS.MS, in.it.TS.N, in.it.TS.Dev, seq, in.body); err != nil {
			return nil, false, err
		}
		out[i] = result{OK: true, Seq: seq}
	}
	if seq == start {
		return out, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET seq = ? WHERE id = ?`, seq, account); err != nil {
		return nil, false, err
	}
	return out, true, tx.Commit()
}

// changes returns items whose change number is after since, oldest change first.
func (s *store) changes(ctx context.Context, account string, since int64, typ string, limit int) ([]json.RawMessage, int64, bool, error) {
	q := `SELECT seq, body FROM items WHERE account = ? AND seq > ?`
	args := []any{account, since}
	if typ != "" {
		q += ` AND type = ?`
		args = append(args, typ)
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	items := []json.RawMessage{}
	next := since
	for rows.Next() {
		var seq int64
		var body []byte
		if err := rows.Scan(&seq, &body); err != nil {
			return nil, 0, false, err
		}
		if len(items) == limit {
			return items, next, true, rows.Err()
		}
		b, err := withSeq(body, seq)
		if err != nil {
			return nil, 0, false, err
		}
		items = append(items, b)
		next = seq
	}
	return items, next, false, rows.Err()
}

// withSeq adds the change number to a stored item.
func withSeq(body []byte, seq int64) (json.RawMessage, error) {
	var it synckosh.Item
	if err := json.Unmarshal(body, &it); err != nil {
		return nil, err
	}
	it.Seq = seq
	return json.Marshal(it)
}
