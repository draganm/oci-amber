package importer

import (
	"math"
	"testing"
)

func TestReportDedupRatio(t *testing.T) {
	r := &Report{Added: 0}
	if _, ok := r.DedupRatio(); ok {
		t.Fatal("want ok = false when Added is 0")
	}
	r = &Report{Compressed: 1000, Added: 250}
	ratio, ok := r.DedupRatio()
	if !ok || ratio != 4 {
		t.Fatalf("DedupRatio() = %v, %v, want 4, true", ratio, ok)
	}
}

func TestReportNotWrittenPercent(t *testing.T) {
	r := &Report{}
	if p := r.NotWrittenPercent(); p != 0 {
		t.Fatalf("NotWrittenPercent() = %v, want 0 when Compressed is 0", p)
	}
	r = &Report{Compressed: 1900727, Added: 1153024}
	if p := r.NotWrittenPercent(); math.Abs(p-39.3) > 0.05 {
		t.Fatalf("NotWrittenPercent() = %v, want ~39.3", p)
	}
	// An all-present re-import writes a bit of manifest metadata but no
	// blobs, so Added can exceed Compressed; the percentage must clamp at 0
	// rather than go negative.
	r = &Report{Compressed: 1000, Added: 1200}
	if p := r.NotWrittenPercent(); p != 0 {
		t.Fatalf("NotWrittenPercent() = %v, want 0 when Added exceeds Compressed", p)
	}
}

func TestReportChunksReusedPercent(t *testing.T) {
	r := &Report{}
	if p := r.ChunksReusedPercent(); p != 0 {
		t.Fatalf("ChunksReusedPercent() = %v, want 0 when Logical is 0", p)
	}
	r = &Report{Logical: 3000000, Deduped: 1146000}
	if p := r.ChunksReusedPercent(); math.Abs(p-38.2) > 0.05 {
		t.Fatalf("ChunksReusedPercent() = %v, want ~38.2", p)
	}
}
