package artifact

import (
	"os"
	"strings"
	"testing"
)

func TestSpillWriterStaysInMemoryUnderThreshold(t *testing.T) {
	w := NewSpillWriter(100)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if w.Spilled() {
		t.Error("should not have spilled yet")
	}
	if string(w.Bytes()) != "hello" {
		t.Errorf("got %q", w.Bytes())
	}
	if w.Total() != 5 {
		t.Errorf("Total() = %d, want 5", w.Total())
	}
}

func TestSpillWriterSpillsPastThreshold(t *testing.T) {
	w := NewSpillWriter(10)
	defer func() {
		if p := w.TempPath(); p != "" {
			os.Remove(p)
		}
	}()

	if _, err := w.Write([]byte("0123456789")); err != nil { // exactly at threshold
		t.Fatal(err)
	}
	if w.Spilled() {
		t.Error("should not spill yet — exactly at threshold, not over")
	}

	if _, err := w.Write([]byte("X")); err != nil { // now over
		t.Fatal(err)
	}
	if !w.Spilled() {
		t.Fatal("should have spilled once threshold was exceeded")
	}
	if w.Total() != 11 {
		t.Errorf("Total() = %d, want 11", w.Total())
	}

	w.Close()
	data, err := os.ReadFile(w.TempPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0123456789X" {
		t.Errorf("spilled file content = %q, want the full write including what was buffered before the spill", data)
	}
}

func TestSpillWriterHandlesOneHugeWrite(t *testing.T) {
	w := NewSpillWriter(10)
	defer func() {
		if p := w.TempPath(); p != "" {
			os.Remove(p)
		}
		w.Close()
	}()

	big := strings.Repeat("a", 1000)
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if !w.Spilled() {
		t.Fatal("a single write far over threshold should spill")
	}
	if w.Total() != 1000 {
		t.Errorf("Total() = %d, want 1000", w.Total())
	}
}

func TestSpillWriterMemoryNeverExceedsThresholdBeforeSpilling(t *testing.T) {
	// The whole point of SpillWriter: the in-memory buffer must never hold
	// more than threshold bytes at any point, even mid-write — this is
	// what actually fixes the unbounded-RAM bug (a size cap applied only
	// *after* Run() returns had already let the buffer grow arbitrarily
	// large while the command was still producing output).
	w := NewSpillWriter(50)
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
		if w.buf.Len() > 50 {
			t.Fatalf("in-memory buffer grew to %d bytes, exceeding the 50-byte threshold", w.buf.Len())
		}
	}
	if !w.Spilled() {
		t.Fatal("1000 bytes written against a 50-byte threshold should have spilled")
	}
	w.Close()
	os.Remove(w.TempPath())
}
