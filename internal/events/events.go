// Package events is a tiny append-only activity log for a participant's box. The
// one-shot CLI commands (import, download) append a line each; a running node
// tails it into its /status so the TUI can show "last import / last download"
// even though those operations happened in separate processes. It is local,
// best-effort observability — never load-bearing, so every call swallows errors
// rather than failing the operation it is recording.
package events

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

// Op is the kind of local operation recorded.
type Op string

const (
	Import   Op = "import"
	Download Op = "download"
	Publish  Op = "publish"
)

// Event is one local operation: what happened, to which assembly, how big.
type Event struct {
	Op       Op        `json:"op"`
	Assembly string    `json:"assembly"`
	Bytes    int64     `json:"bytes"`
	Segments int       `json:"segments"`
	Note     string    `json:"note,omitempty"` // e.g. "delta vs TAIR10", "from peers"
	Time     time.Time `json:"time"`
}

// Append writes one event as a JSON line to path, creating it if needed. Errors
// are swallowed: a failed log write must never fail the import/download itself.
func Append(path string, ev Event) {
	if path == "" {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if data, err := json.Marshal(ev); err == nil {
		f.Write(append(data, '\n'))
	}
}

// Tail returns the last n events in chronological order (oldest first). A missing
// or unreadable log yields nil, no error — the caller treats "no history" the
// same as "log not there yet".
func Tail(path string, n int) []Event {
	if path == "" || n <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Ring buffer of the last n decoded events; cheap for a small log and bounded
	// memory for a large one.
	buf := make([]Event, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if len(buf) < n {
			buf = append(buf, ev)
		} else {
			copy(buf, buf[1:])
			buf[n-1] = ev
		}
	}
	return buf
}
