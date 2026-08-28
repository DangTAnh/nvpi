package nvidia

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sseDebug captures raw upstream SSE lines (OpenAI shape, before coalesce
// and SDK translate) plus post-SDK Anthropic payloads, one file per
// streaming turn. Enabled with -debug-sse-dir=<dir>; disabled by default
// to avoid the per-turn open(2)/write(2) cost on the hot path.
type sseDebug struct {
	dir  string
	mu   sync.Mutex
	raw  *os.File
	anth *os.File
}

func startSseDebug(dir, model, reqID string) *sseDebug {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	stamp := time.Now().UnixNano()
	base := fmt.Sprintf("%d-%s.log", stamp, sanitizeName(model))
	rf, rerr := os.Create(filepath.Join(dir, "raw-"+base))
	af, aerr := os.Create(filepath.Join(dir, "anth-"+base))
	if rerr != nil || aerr != nil {
		if rf != nil {
			rf.Close()
		}
		if af != nil {
			af.Close()
		}
		return nil
	}
	header := fmt.Sprintf("# model=%s reqID=%s started=%s\n", model, reqID, time.Now().Format(time.RFC3339Nano))
	rf.WriteString(header)
	af.WriteString(header)
	return &sseDebug{dir: dir, raw: rf, anth: af}
}

func (d *sseDebug) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.raw != nil {
		d.raw.Close()
		d.raw = nil
	}
	if d.anth != nil {
		d.anth.Close()
		d.anth = nil
	}
}

func (d *sseDebug) writeRaw(line string) {
	if d == nil || d.raw == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.raw.WriteString(line)
	if len(line) == 0 || line[len(line)-1] != '\n' {
		d.raw.Write([]byte{'\n'})
	}
}

func (d *sseDebug) writeAnthropic(payload []byte) {
	if d == nil || d.anth == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.anth.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		d.anth.Write([]byte{'\n'})
	}
}

func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "model"
	}
	return string(out)
}
