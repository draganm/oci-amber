package tui

import (
	"fmt"
	"io"
	"time"

	"github.com/draganm/oci-amber/importer"
)

// RunPlain drives run without a screen: a status line is written to w every
// interval until run returns. It is what a non-terminal stdout gets.
func RunPlain(w io.Writer, tr *importer.Tracker, interval time.Duration, run func() (*importer.Report, error)) (*importer.Report, error) {
	type result struct {
		rep *importer.Report
		err error
	}
	results := make(chan result, 1)
	go func() {
		rep, err := run()
		results <- result{rep, err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case r := <-results:
			return r.rep, r.err
		case <-ticker.C:
			fmt.Fprintln(w, StatusLine(tr.Snapshot()))
		}
	}
}
