package artifact

import (
	"bytes"
	"os"
)

// SpillWriter is an io.Writer that buffers up to threshold bytes in memory,
// then — the moment a write would cross that line — switches to streaming
// straight to a temp file for everything after. This exists to fix a real
// bug class: a tool that captured output as `var out bytes.Buffer` and
// only checked a size cap *after* cmd.Run() returned had already let an
// unbounded amount of memory accumulate for however long the command
// produced output. Capping only the *reported* size never capped the
// actual RAM used to get there. Using SpillWriter as cmd.Stdout/Stderr
// bounds memory use for the entire lifetime of the command, not just the
// final result.
type SpillWriter struct {
	threshold int
	buf       bytes.Buffer
	file      *os.File
	total     int64
}

// NewSpillWriter returns a writer that keeps up to threshold bytes in
// memory before spilling the rest to disk.
func NewSpillWriter(threshold int) *SpillWriter {
	return &SpillWriter{threshold: threshold}
}

func (w *SpillWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))

	if w.file == nil && w.buf.Len()+len(p) > w.threshold {
		f, err := os.CreateTemp("", "kram-spill-*")
		if err != nil {
			return 0, err
		}
		if w.buf.Len() > 0 {
			if _, err := f.Write(w.buf.Bytes()); err != nil {
				f.Close()
				os.Remove(f.Name())
				return 0, err
			}
			w.buf.Reset()
		}
		w.file = f
	}

	if w.file != nil {
		return w.file.Write(p)
	}
	return w.buf.Write(p)
}

// Spilled reports whether output crossed the threshold and moved to disk.
func (w *SpillWriter) Spilled() bool { return w.file != nil }

// Bytes returns everything written so far, if it never spilled. Calling it
// after Spilled() is true returns nothing useful — read TempPath() instead.
func (w *SpillWriter) Bytes() []byte { return w.buf.Bytes() }

// TempPath is the spill file's path once Spilled() is true, "" otherwise.
// The caller owns cleanup: either persist it into the artifact store
// (Store.SaveFile removes the source on success) or remove it directly.
func (w *SpillWriter) TempPath() string {
	if w.file == nil {
		return ""
	}
	return w.file.Name()
}

// Total is how many bytes were written in all, spilled or not.
func (w *SpillWriter) Total() int64 { return w.total }

// Close closes (but does not remove) the underlying temp file, if any —
// callers that don't persist the spill into the artifact store should
// os.Remove(w.TempPath()) themselves after Close.
func (w *SpillWriter) Close() error {
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
