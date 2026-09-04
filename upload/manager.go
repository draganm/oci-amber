package upload

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var errClosed = errors.New("upload: manager closed")

// Manager owns the upload sessions of one registry process. Sessions live
// in a mutex-protected map; spilled sessions keep their file under dir,
// named by the session id. A background sweeper removes sessions that have
// been idle for longer than the timeout together with their files.
type Manager struct {
	dir         string
	maxInMemory int64
	timeout     time.Duration
	log         *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool

	stop chan struct{}
	done chan struct{}
}

// NewManager creates dir if needed, deletes everything left in it from a
// previous process, and starts the sweeper. dir belongs to the manager:
// only its contents are ever removed, never the directory itself, and the
// caller must not point it at a directory it shares with anything else
// (cmd/oci-amber puts it under <work-dir>/oci-amber/uploads for exactly
// that reason). maxInMemory is the number of
// bytes a session keeps in memory before spilling (0 spills immediately)
// and timeout is the idle time after which a session expires; it must be
// positive. A nil log uses slog.Default().
func NewManager(dir string, maxInMemory int64, timeout time.Duration, log *slog.Logger) (*Manager, error) {
	if maxInMemory < 0 {
		return nil, fmt.Errorf("upload: max-in-memory must not be negative, got %d", maxInMemory)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("upload: timeout must be positive, got %v", timeout)
	}
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("upload: creating %s: %w", dir, err)
	}
	removed, err := emptyDir(dir)
	if err != nil {
		return nil, fmt.Errorf("upload: emptying %s: %w", dir, err)
	}
	if removed > 0 {
		log.Info("removed leftover upload files", "dir", dir, "count", removed)
	}
	m := &Manager{
		dir:         dir,
		maxInMemory: maxInMemory,
		timeout:     timeout,
		log:         log,
		sessions:    map[string]*Session{},
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go m.sweeper(sweepInterval(timeout))
	return m, nil
}

// emptyDir removes every entry of dir, leaving dir itself in place, and
// returns how many entries it removed.
func emptyDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return 0, err
		}
	}
	return len(entries), nil
}

// sweepInterval is a tenth of the idle timeout, clamped to [10ms, 1m].
func sweepInterval(timeout time.Duration) time.Duration {
	return max(10*time.Millisecond, min(time.Minute, timeout/10))
}

func (m *Manager) sweeper(interval time.Duration) {
	defer close(m.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-t.C:
			m.Sweep(now)
		}
	}
}

// newID returns 16 random bytes as 32 lowercase hex characters.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Create registers a new empty session.
func (m *Manager) Create() (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("upload: generating session id: %w", err)
	}
	s := newSession(id, filepath.Join(m.dir, id), m.maxInMemory, time.Now())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errClosed
	}
	m.sessions[id] = s
	m.log.Debug("upload session created", "id", id)
	return s, nil
}

// Get returns the session with the given id and records activity on it. It
// returns ErrUnknown when there is no such session.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrUnknown
	}
	s.touch()
	return s, nil
}

// Remove unregisters the session and deletes its file, closing the file
// before forgetting the session, the same ordering Sweep uses. It returns
// ErrUnknown when there is no such session. If removing the file fails,
// the session stays registered (unusable for Append and Spool, since
// close already marks it closed) so a later Remove, or Sweep once it goes
// idle, can retry; the error is returned to the caller either way.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	size := s.Offset()
	if err := s.close(); err != nil {
		m.log.Error("removing upload session", "id", id, "path", s.path, "error", err)
		return err
	}
	m.mu.Lock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	m.log.Debug("upload session removed", "id", id, "size", size)
	return nil
}

// Sweep removes every session whose last activity is more than the timeout
// before now, together with its file, and returns how many it expired. A
// session's file is always removed before the session is forgotten (dropped
// from the map), so once a caller observes a session gone from the map, its
// file is already gone too.
//
// Idleness is checked twice against the same deadline: once to collect the
// candidates and once more after Offset has returned. Offset waits for the
// session mutex, which an Append holds for the whole copy of a request
// body, so between the two checks an arbitrary amount of time can pass and
// the session may have received bytes (every write touches last-active). A
// session that became active again in that window is left alone: it is
// neither closed nor forgotten, and the next sweep considers it again.
func (m *Manager) Sweep(now time.Time) int {
	m.mu.Lock()
	var expired []*Session
	for _, s := range m.sessions {
		if now.Sub(s.active()) > m.timeout {
			expired = append(expired, s)
		}
	}
	m.mu.Unlock()
	swept := 0
	for _, s := range expired {
		size := s.Offset()
		if now.Sub(s.active()) <= m.timeout {
			m.log.Debug("upload session became active again, not expiring", "id", s.ID, "size", size)
			continue
		}
		swept++
		if err := s.close(); err != nil {
			m.log.Error("removing expired upload session", "id", s.ID, "path", s.path, "error", err)
			continue
		}
		m.log.Info("upload session expired", "id", s.ID, "size", size)
		m.mu.Lock()
		if m.sessions[s.ID] == s {
			delete(m.sessions, s.ID)
		}
		m.mu.Unlock()
	}
	return swept
}

// Close stops the sweeper, attempts to remove every session's file and
// forgets every session. Unlike Sweep and Remove, Close drops a session
// from the map even when removing its file failed: Close is terminal, so
// the map is unreachable to any other caller once it returns regardless,
// and there is no later Sweep or Remove that could retry. Instead, each
// removal failure is logged at error level together with the session's
// path and returned (joined with any others) from Close, so a caller such
// as process shutdown can report or act on files left behind. Create
// fails afterwards. Close is idempotent: a second call does no work and
// returns nil, even if the first call reported errors.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	close(m.stop)
	<-m.done
	var errs []error
	for _, s := range sessions {
		if err := s.close(); err != nil {
			m.log.Error("removing upload session file", "id", s.ID, "path", s.path, "error", err)
			errs = append(errs, err)
		}
		m.mu.Lock()
		if m.sessions[s.ID] == s {
			delete(m.sessions, s.ID)
		}
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}
