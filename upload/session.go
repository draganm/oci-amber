package upload

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"sync"
	"time"

	"github.com/draganm/oci-amber/oci"
)

// ErrUnknown is returned for an upload session id that is not registered,
// which includes sessions that were removed, expired or closed with the
// manager.
var ErrUnknown = errors.New("upload: unknown session")

// Session is one in-progress blob upload. Bytes are appended in request
// order. They stay in a memory buffer while the total is at or below
// maxInMemory; the first append that would exceed it creates the session's
// file, writes the buffer out, drops the buffer and appends to the file from
// then on. A sha256 runs over every byte received.
//
// All methods are safe for concurrent use. Requests on the same session are
// serialized on its mutex, so overlapping appends see consistent offsets.
type Session struct {
	// ID is the session's random 128-bit id in lowercase hex.
	ID string

	path        string
	maxInMemory int64

	mu         sync.Mutex
	buf        bytes.Buffer
	file       *os.File
	size       int64
	hash       hash.Hash
	lastActive time.Time
	closed     bool
}

func newSession(id, path string, maxInMemory int64, now time.Time) *Session {
	return &Session{
		ID:          id,
		path:        path,
		maxInMemory: maxInMemory,
		hash:        sha256.New(),
		lastActive:  now,
	}
}

// Append reads r until EOF, appends its bytes to the session and returns the
// new total byte count. When r fails part way the bytes read before the
// failure stay in the session and the returned offset counts them. On a
// removed session it returns ErrUnknown.
func (s *Session) Append(r io.Reader) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()
	if s.closed {
		return s.size, ErrUnknown
	}
	_, err := io.Copy(sessionWriter{s}, r)
	return s.size, err
}

// Offset is the number of bytes appended so far.
func (s *Session) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Spool returns a snapshot of everything appended so far together with its
// sha256 digest. The session keeps its data and stays registered so that a
// failed finalize can be retried; the caller removes the session through
// Manager.Remove once the blob is stored. A memory spool shares the
// session's buffer, which is only ever appended to, so later appends do not
// change what the spool reads. On a removed session it returns ErrUnknown.
func (s *Session) Spool() (*Spool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()
	if s.closed {
		return nil, ErrUnknown
	}
	sp := &Spool{
		size:   s.size,
		digest: oci.DigestFromSum(s.hash.Sum(nil)),
		touch:  s.touch,
	}
	if s.file != nil {
		sp.path = s.path
	} else {
		sp.mem = s.buf.Bytes()
	}
	return sp, nil
}

// sessionWriter adapts a session to io.Writer for io.Copy. The caller of
// Append holds the session mutex for the whole copy.
type sessionWriter struct{ s *Session }

func (w sessionWriter) Write(p []byte) (int, error) { return w.s.write(p) }

// write appends p, spilling the buffer to the file first when p would push
// the in-memory total over maxInMemory. The caller holds mu.
func (s *Session) write(p []byte) (int, error) {
	if s.file == nil && s.size+int64(len(p)) > s.maxInMemory {
		if err := s.spill(); err != nil {
			return 0, err
		}
	}
	if s.file != nil {
		n, err := s.file.Write(p)
		s.hash.Write(p[:n])
		s.size += int64(n)
		return n, err
	}
	n, _ := s.buf.Write(p)
	s.hash.Write(p[:n])
	s.size += int64(n)
	return n, nil
}

// spill creates the session's file, writes the buffered bytes to it and
// drops the buffer. The caller holds mu.
func (s *Session) spill() error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(s.buf.Bytes()); err != nil {
		f.Close()
		os.Remove(s.path)
		return err
	}
	s.file = f
	s.buf = bytes.Buffer{}
	return nil
}

// touch records activity on the session.
func (s *Session) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// active returns the time of the last activity on the session.
func (s *Session) active() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive
}

// close releases the buffer, closes and deletes the file and marks the
// session removed. It is idempotent.
func (s *Session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.buf = bytes.Buffer{}
	var errs []error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			errs = append(errs, err)
		}
		s.file = nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
