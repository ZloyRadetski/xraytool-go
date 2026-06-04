// Package userdb manages the limited_users.db file — a flat text file listing
// blocked users in the format:
//
//	email|subfile|limit
//
// The format is kept intentionally compatible with sub.php so that the PHP
// subscription handler doesn't require changes.
package userdb

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Entry represents a single blocked-user record.
type Entry struct {
	Email   string
	Subfile string
	Limit   *float64 // nil means no limit stored
}

// DB is a mutex-protected limited_users database.
type DB struct {
	path string
	mu   sync.Mutex
}

// New creates a new DB handle. The file is created lazily on first write.
func New(path string) *DB {
	return &DB{path: path}
}

// Exists returns true if the email has a record in the DB.
func (db *DB) Exists(email string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	entries, err := db.read()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Email == email {
			return true, nil
		}
	}
	return false, nil
}

// Get returns the entry for email, or nil if not found.
func (db *DB) Get(email string) (*Entry, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	entries, err := db.read()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Email == email {
			cp := e
			return &cp, nil
		}
	}
	return nil, nil
}

// All returns all entries in the DB.
func (db *DB) All() ([]Entry, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.read()
}

// Upsert adds or replaces the record for entry.Email.
func (db *DB) Upsert(entry Entry) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	entries, err := db.read()
	if err != nil {
		return err
	}

	found := false
	for i, e := range entries {
		if e.Email == entry.Email {
			entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}
	return db.write(entries)
}

// Remove deletes the record for email. No-op if not found.
func (db *DB) Remove(email string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	entries, err := db.read()
	if err != nil {
		return err
	}

	n := 0
	for _, e := range entries {
		if e.Email != email {
			entries[n] = e
			n++
		}
	}
	return db.write(entries[:n])
}

// UpdateLimit changes the limit field for an existing entry.
// Returns an error if the email is not in the DB.
func (db *DB) UpdateLimit(email string, limit *float64) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	entries, err := db.read()
	if err != nil {
		return err
	}
	for i, e := range entries {
		if e.Email == email {
			entries[i].Limit = limit
			return db.write(entries)
		}
	}
	return fmt.Errorf("user %q not found in limited db", email)
}

// ---------------------------------------------------------------------------
// Internal I/O
// ---------------------------------------------------------------------------

func (db *DB) read() ([]Entry, error) {
	if _, err := os.Stat(db.path); os.IsNotExist(err) {
		return nil, nil
	}
	f, err := os.Open(db.path)
	if err != nil {
		return nil, fmt.Errorf("opening limited db: %w", err)
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		email := strings.TrimSpace(parts[0])
		if email == "" {
			continue
		}
		subfile := ""
		if len(parts) > 1 {
			subfile = strings.TrimSpace(parts[1])
		}
		if subfile == "" {
			subfile = "unknown.txt"
		}
		e := Entry{Email: email, Subfile: subfile}
		if len(parts) > 2 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
				e.Limit = &v
			}
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading limited db: %w", err)
	}
	return entries, nil
}

func (db *DB) write(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(db.path), 0o755); err != nil {
		return fmt.Errorf("creating limited db dir: %w", err)
	}

	var sb strings.Builder
	for _, e := range entries {
		if e.Email == "" {
			continue
		}
		sub := e.Subfile
		if sub == "" {
			sub = "unknown.txt"
		}
		if e.Limit != nil {
			fmt.Fprintf(&sb, "%s|%s|%.0f\n", e.Email, sub, *e.Limit)
		} else {
			fmt.Fprintf(&sb, "%s|%s\n", e.Email, sub)
		}
	}

	tmpPath := db.path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp file: %w", err)
	}
	f.Close()
	if err := os.Rename(tmpPath, db.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing limited db: %w", err)
	}
	return nil
}
