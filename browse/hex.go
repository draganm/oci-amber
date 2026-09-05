package browse

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// hexRowBytes is how many bytes one hex row shows.
const hexRowBytes = 16

// RenderHex renders data, which starts at offset in the file, as rows of
// 16 bytes: an eight-digit hex offset, two groups of eight hex bytes and
// the printable ASCII with '.' for the rest. A short last row is padded
// so the ASCII column stays aligned. Rows are cut to width.
func RenderHex(offset int64, data []byte, width int) string {
	var rows []string
	for start := 0; start < len(data); start += hexRowBytes {
		row := data[start:min(start+hexRowBytes, len(data))]
		var hex, asc strings.Builder
		for i := 0; i < hexRowBytes; i++ {
			if i == 8 {
				hex.WriteByte(' ')
			}
			if i >= len(row) {
				hex.WriteString("   ")
				continue
			}
			fmt.Fprintf(&hex, "%02x ", row[i])
			if c := row[i]; c >= 0x20 && c < 0x7f {
				asc.WriteByte(c)
			} else {
				asc.WriteByte('.')
			}
		}
		rows = append(rows, truncate(fmt.Sprintf("%08x  %s %s", offset+int64(start), hex.String(), asc.String()), width))
	}
	return strings.Join(rows, "\n")
}

// readWindow reads up to length bytes of f starting at start, fewer at
// the end of the file and none past it. Whole chunks before start are
// skipped by their key lengths alone, so a window deep inside a large
// file fetches about one chunk plus the window.
func readWindow(f *File, start, length int64) ([]byte, error) {
	if start < 0 || start >= f.Size || length <= 0 {
		return nil, nil
	}
	r := f.Open()
	defer r.Close()
	if err := r.Skip(start); err != nil {
		return nil, fmt.Errorf("browse: seeking %s to %d: %w", f.Name, start, err)
	}
	buf := make([]byte, min(length, f.Size-start))
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("browse: reading %s at %d: %w", f.Name, start, err)
	}
	return buf[:n], nil
}
