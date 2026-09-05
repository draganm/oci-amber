package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// MaxTextSize is the largest file the text mode reads whole; bigger files
// open in hex only.
const MaxTextSize = 8 << 20

// probeSize is how much of a file's head decides between text and hex.
const probeSize = 8 << 10

// Kind is what the viewer shows a file as.
type Kind int

const (
	KindText     Kind = iota // UTF-8 text, shown with line numbers
	KindBinary               // anything else, shown as a hex dump; h switches to text
	KindTooLarge             // over MaxTextSize: hex only
)

// Classification is the viewer's verdict on a file.
type Classification struct {
	Kind  Kind
	Label string // json, yaml, shell, toml, markdown, text or binary
}

// Classify decides how to show a file of size bytes from its name and a
// probe of its first bytes: too large is hex only; a NUL byte or invalid
// UTF-8 is binary; the rest is text, labelled by extension or shebang.
// The JSON label is decided later, by LoadText, once the whole file is
// read. A probe shorter than the file may end inside a multi-byte rune,
// which is not a fault.
func Classify(name string, size int64, probe []byte) Classification {
	if size > MaxTextSize {
		return Classification{Kind: KindTooLarge, Label: "binary"}
	}
	if bytes.IndexByte(probe, 0) >= 0 || !validUTF8Prefix(probe, int64(len(probe)) < size) {
		return Classification{Kind: KindBinary, Label: "binary"}
	}
	return Classification{Kind: KindText, Label: labelFor(name, probe)}
}

// validUTF8Prefix reports whether p is valid UTF-8, ignoring an incomplete
// rune at its end when truncated says more bytes follow it.
func validUTF8Prefix(p []byte, truncated bool) bool {
	if truncated {
		for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
			if utf8.RuneStart(p[i]) {
				if !utf8.FullRune(p[i:]) {
					p = p[:i]
				}
				break
			}
		}
	}
	return utf8.Valid(p)
}

// labelFor names a text file's type from its extension, else its shebang.
func labelFor(name string, head []byte) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".sh", ".bash":
		return "shell"
	case ".toml":
		return "toml"
	case ".md":
		return "markdown"
	}
	if bytes.HasPrefix(head, []byte("#!")) {
		return "shell"
	}
	return "text"
}

// Text is a file loaded for the text mode.
type Text struct {
	Label  string
	Raw    []byte // the stored bytes
	Pretty []byte // indented JSON; nil unless the file is JSON
}

// LoadText wraps data; when json.Valid accepts it the label becomes json
// and Pretty holds it indented by two spaces with key order preserved.
func LoadText(label string, data []byte) *Text {
	t := &Text{Label: label, Raw: data}
	if len(bytes.TrimSpace(data)) > 0 && json.Valid(data) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err == nil {
			t.Label = "json"
			t.Pretty = buf.Bytes()
		}
	}
	return t
}

// Lines splits data into display lines: "\n" separates, a trailing "\r"
// is dropped, a tab becomes four spaces, other control characters render
// as ^X and bytes that are not UTF-8 as \xNN. An empty file has no lines
// and a trailing newline adds none.
func Lines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = sanitize(bytes.TrimSuffix(p, []byte("\r")))
	}
	return lines
}

// sanitize renders one line's bytes as printable text.
func sanitize(p []byte) string {
	var b strings.Builder
	for len(p) > 0 {
		r, n := utf8.DecodeRune(p)
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, p[0])
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
			b.WriteByte('^')
			b.WriteByte(byte(r) ^ 0x40)
		default:
			b.WriteRune(r)
		}
		p = p[n:]
	}
	return b.String()
}

// lineOffset is the byte offset in data where line n (0-based) starts;
// past the last line it is len(data).
func lineOffset(data []byte, n int) int64 {
	off := 0
	for ; n > 0; n-- {
		i := bytes.IndexByte(data[off:], '\n')
		if i < 0 {
			return int64(len(data))
		}
		off += i + 1
	}
	return int64(off)
}

// lineAt is the 0-based line that holds byte offset off; past the end it
// is the last line.
func lineAt(data []byte, off int64) int {
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	return bytes.Count(data[:off], []byte("\n"))
}
