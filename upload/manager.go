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
// previous process, and starts the sweeper. maxInMemory is the number of
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

// emptyDir removes every entry of dir and returns how many it removed.
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

// Remove unregisters the session and deletes its file. It returns
// ErrUnknown when there is no such session.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	m.log.Debug("upload session removed", "id", id, "size", s.Offset())
	return s.close()
}

// Sweep removes every session whose last activity is more than the timeout
// before now, together with its file, and returns how many it removed.
func (m *Manager) Sweep(now time.Time) int {
	m.mu.Lock()
	var expired []*Session
	for id, s := range m.sessions {
		if now.Sub(s.active()) > m.timeout {
			delete(m.sessions, id)
			expired = append(expired, s)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		size := s.Offset()
		if err := s.close(); err != nil {
			m.log.Error("removing expired upload session", "id", s.ID, "error", err)
			continue
		}
		m.log.Info("upload session expired", "id", s.ID, "size", size)
	}
	return len(expired)
}

// Close stops the sweeper and removes every session and its file. Create
// fails afterwards. Close is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := m.sessions
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	close(m.stop)
	<-m.done
	var errs []error
	for _, s := range sessions {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
