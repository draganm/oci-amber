package tui

import (
	"errors"
	"testing"
)

func TestTerminalErrorErrorAndUnwrap(t *testing.T) {
	cause := errors.New("open /dev/tty: not a terminal")
	err := &TerminalError{Err: cause}
	if got, want := err.Error(), "terminal: open /dev/tty: not a terminal"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(err, cause) = false, want true through Unwrap")
	}
	joined := errors.Join(err, errors.New("import: some other failure"))
	var terr *TerminalError
	if !errors.As(joined, &terr) || terr != err {
		t.Fatalf("errors.As did not recover the TerminalError from the joined error")
	}
}
