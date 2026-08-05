package zka

import (
	"bytes"
	"sync"
	"testing"
)

type recordingChunkWriter struct {
	mu     sync.Mutex
	writes []int
	data   bytes.Buffer
}

func (w *recordingChunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, len(p))
	return w.data.Write(p)
}

func TestChunkedWriterBoundsLargeControlWrites(t *testing.T) {
	payload := bytes.Repeat([]byte("workspace-snapshot"), 10000)
	recorder := &recordingChunkWriter{}
	written, err := (chunkedWriter{w: recorder}).Write(payload)
	if err != nil || written != len(payload) {
		t.Fatalf("write = %d, %v", written, err)
	}
	if !bytes.Equal(recorder.data.Bytes(), payload) {
		t.Fatal("chunking changed the control payload")
	}
	if len(recorder.writes) < 2 {
		t.Fatalf("large payload used %d write", len(recorder.writes))
	}
	for _, size := range recorder.writes {
		if size > 32<<10 {
			t.Fatalf("control chunk = %d bytes", size)
		}
	}
}
