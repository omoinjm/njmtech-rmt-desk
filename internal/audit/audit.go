// Package audit writes an append-only JSON-lines log of security events.
package audit

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // "connect","deny","disconnect","file_recv",...
	Peer   string    `json:"peer"`
	Remote string    `json:"remote,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

type Log struct {
	mu sync.Mutex
	f  *os.File
}

func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

func (l *Log) Write(e Event) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

func (l *Log) Close() error { return l.f.Close() }
