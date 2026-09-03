package upload

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T, maxInMemory int64, timeout time.Duration) (*Manager, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	m, err := NewManager(dir, maxInMemory, timeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m, dir
}

func setLastActive(s *Session, at time.Time) {
	s.mu.Lock()
	s.lastActive = at
	s.mu.Unlock()
}

// sessionCount reads the registry size without touching any session.
func sessionCount(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func TestManagerCreateGetRemove(t *testing.T) {
	m, dir := newTestManager(t, 16, time.Hour)
	s, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(s.ID) != 32 {
		t.Fatalf("ID %q has length %d, want 32", s.ID, len(s.ID))
	}
	if _, err := hex.DecodeString(s.ID); err != nil {
		t.Fatalf("ID %q is not hex: %v", s.ID, err)
	}
	s2, err := m.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s2.ID == s.ID {
		t.Fatalf("two sessions share id %s", s.ID)
	}

	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != s {
		t.Fatal("Get returned a different session")
	}

	appendBytes(t, s, pattern(100, 1))
	if _, err := os.Stat(filepath.Join(dir, s.ID)); err != nil {
		t.Fatalf("spilled file missing: %v", err)
	}
	appendBytes(t, s2, pattern(5, 2))

	if err := m.Remove(s.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, s.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present after Remove: %v", err)
	}
	if _, err := m.Get(s.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Get after Remove = %v, want ErrUnknown", err)
	}
	if err := m.Remove(s.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("second Remove = %v, want ErrUnknown", err)
	}
	if _, err := s.Append(bytes.NewReader([]byte("x"))); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Append on removed session = %v, want ErrUnknown", err)
	}
	if _, err := s.Spool(); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Spool on removed session = %v, want ErrUnknown", err)
	}

	// The other session is untouched.
	if got, err := m.Get(s2.ID); err != nil || got != s2 || got.Offset() != 5 {
		t.Fatalf("Get(s2) = %v, %v", got, err)
	}
}

func TestManagerGetUnknown(t *testing.T) {
	m, _ := newTestManager(t, 16, time.Hour)
	if _, err := m.Get("does-not-exist"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Get = %v, want ErrUnknown", err)
	}
	if err := m.Remove("does-not-exist"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Remove = %v, want ErrUnknown", err)
	}
}

func TestNewManagerCreatesDirAndEmptiesLeftovers(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b", "uploads")
		m, err := NewManager(dir, 16, time.Hour, nil)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		defer m.Close()
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("dir not created: %v", err)
		}
	})
	t.Run("leftovers", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "uploads")
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"old1", "old2", filepath.Join("sub", "nested")} {
			if err := os.WriteFile(filepath.Join(dir, p), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var logs bytes.Buffer
		m, err := NewManager(dir, 16, time.Hour, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		defer m.Close()
		if names := dirNames(t, dir); len(names) != 0 {
			t.Fatalf("leftovers survived startup: %v", names)
		}
		if !strings.Contains(logs.String(), "count=3") {
			t.Fatalf("startup cleanup was not logged: %q", logs.String())
		}
	})
}

func TestNewManagerRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewManager(dir, 16, 0, nil); err == nil {
		t.Fatal("NewManager accepted a zero timeout")
	}
	if _, err := NewManager(dir, -1, time.Hour, nil); err == nil {
		t.Fatal("NewManager accepted a negative max-in-memory")
	}
}

func TestSweepRemovesIdleSessions(t *testing.T) {
	m, dir := newTestManager(t, 16, time.Hour)
	spilled, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	appendBytes(t, spilled, pattern(100, 1))
	inMemory, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	appendBytes(t, inMemory, pattern(10, 2))
	fresh, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(time.Now()); n != 0 {
		t.Fatalf("Sweep removed %d fresh sessions", n)
	}

	past := time.Now().Add(-2 * time.Hour)
	for _, s := range []*Session{spilled, inMemory, fresh} {
		setLastActive(s, past)
	}
	// Get counts as activity.
	if _, err := m.Get(fresh.ID); err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(time.Now()); n != 2 {
		t.Fatalf("Sweep removed %d sessions, want 2", n)
	}
	if _, err := m.Get(spilled.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("spilled session still known: %v", err)
	}
	if _, err := m.Get(inMemory.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("in-memory session still known: %v", err)
	}
	if _, err := m.Get(fresh.ID); err != nil {
		t.Fatalf("fresh session was swept: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("expired session files left behind: %v", names)
	}
	if _, err := spilled.Append(bytes.NewReader([]byte("x"))); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Append on expired session = %v, want ErrUnknown", err)
	}
	if n := m.Sweep(time.Now()); n != 0 {
		t.Fatalf("second Sweep removed %d sessions", n)
	}
}

func TestSweepBoundary(t *testing.T) {
	const timeout = time.Hour
	m, _ := newTestManager(t, 16, timeout)
	s, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	setLastActive(s, t0)
	if n := m.Sweep(t0.Add(timeout)); n != 0 {
		t.Fatalf("Sweep at exactly the timeout removed %d", n)
	}
	if n := m.Sweep(t0.Add(timeout + time.Nanosecond)); n != 1 {
		t.Fatalf("Sweep just past the timeout removed %d, want 1", n)
	}
}

func TestAppendAndSpoolCountAsActivity(t *testing.T) {
	m, _ := newTestManager(t, 10, time.Hour)
	past := time.Now().Add(-2 * time.Hour)

	s, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	setLastActive(s, past)
	appendBytes(t, s, pattern(100, 1))
	if n := m.Sweep(time.Now()); n != 0 {
		t.Fatalf("Sweep removed a session that just received an Append")
	}

	setLastActive(s, past)
	sp, err := s.Spool()
	if err != nil {
		t.Fatal(err)
	}
	if n := m.Sweep(time.Now()); n != 0 {
		t.Fatalf("Sweep removed a session that just produced a Spool")
	}

	// Opening the spool during finalization keeps the session alive too.
	setLastActive(s, past)
	r, err := sp.Open()
	if err != nil {
		t.Fatal(err)
	}
	r.(io.Closer).Close()
	if n := m.Sweep(time.Now()); n != 0 {
		t.Fatalf("Sweep removed a session whose spool was just opened")
	}
}

func TestBackgroundSweeper(t *testing.T) {
	m, dir := newTestManager(t, 16, 100*time.Millisecond)
	s, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	appendBytes(t, s, pattern(100, 1))
	// Poll without Get, which would count as activity and keep the session
	// alive.
	deadline := time.Now().Add(10 * time.Second)
	for sessionCount(m) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session was not expired by the background sweeper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Get(s.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Get after expiry = %v, want ErrUnknown", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("expired session file left behind: %v", names)
	}
}

func TestManagerCloseRemovesAll(t *testing.T) {
	m, dir := newTestManager(t, 16, time.Hour)
	spilled, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	appendBytes(t, spilled, pattern(100, 1))
	inMemory, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	appendBytes(t, inMemory, pattern(5, 2))

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("files left after Close: %v", names)
	}
	for _, s := range []*Session{spilled, inMemory} {
		if _, err := m.Get(s.ID); !errors.Is(err, ErrUnknown) {
			t.Fatalf("Get after Close = %v, want ErrUnknown", err)
		}
		if _, err := s.Append(bytes.NewReader([]byte("x"))); !errors.Is(err, ErrUnknown) {
			t.Fatalf("Append after Close = %v, want ErrUnknown", err)
		}
	}
	if _, err := m.Create(); err == nil {
		t.Fatal("Create after Close succeeded")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
