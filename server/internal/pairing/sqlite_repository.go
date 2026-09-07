package pairing

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteRepository keeps the existing in-process authority while persisting
// user/device bindings across a single-server restart.
type SQLiteRepository struct {
	*MemoryRepository
	db *sql.DB
}

func NewSQLiteRepository(path string, now func() time.Time) (*SQLiteRepository, error) {
	if path == "" {
		path = "build/stackchan.db"
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	r := &SQLiteRepository{MemoryRepository: NewMemoryRepository(now), db: db}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS stackchan_bindings (mac TEXT PRIMARY KEY, user_id TEXT NOT NULL, bound_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	rows, err := db.Query(`SELECT mac, user_id, bound_at FROM stackchan_bindings`)
	if err != nil {
		db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mac, user string
		var bound int64
		if err := rows.Scan(&mac, &user, &bound); err != nil {
			db.Close()
			return nil, err
		}
		r.owners[mac] = user
		if r.devices[user] == nil {
			r.devices[user] = map[string]Device{}
		}
		r.devices[user][mac] = Device{MAC: mac, BoundAt: bound}
	}
	return r, rows.Err()
}

func (r *SQLiteRepository) Bind(userID, mac, nonce string, generation uint64) error {
	if err := r.MemoryRepository.Bind(userID, mac, nonce, generation); err != nil {
		return err
	}
	r.mu.Lock()
	device := r.devices[userID][mac]
	r.mu.Unlock()
	_, err := r.db.Exec(`INSERT INTO stackchan_bindings(mac,user_id,bound_at) VALUES(?,?,?) ON CONFLICT(mac) DO UPDATE SET user_id=excluded.user_id,bound_at=excluded.bound_at`, mac, userID, device.BoundAt)
	return err
}

func (r *SQLiteRepository) Unbind(userID, mac string) bool {
	if !r.MemoryRepository.Unbind(userID, mac) {
		return false
	}
	_, _ = r.db.Exec(`DELETE FROM stackchan_bindings WHERE mac=? AND user_id=?`, mac, userID)
	return true
}

func (r *SQLiteRepository) Close() error { return r.db.Close() }
