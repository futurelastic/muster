package opencode

import (
	"context"
	"regexp"
	"sync"
	"time"
)

// logCapture holds the tail of one runtime process's own log (muster #284).
//
// # Why this exists
//
// A model or configuration error reaches the HTTP API only as
// `UnknownError "Unexpected server error"` plus a reference. The real cause —
// a provider that does not know the model, say — is written to the server's
// own log and nowhere else, and the server prints that log only when started
// with --print-logs (process.go). Without it a create-time failure stays
// opaque to everything that reads this driver.
//
// # What it keeps, and what it never lets out
//
// A bounded tail of lines, in memory, per process; it dies with the process.
// The lines are NEVER returned to a caller and never written anywhere: the one
// thing taken from them is an error NAME (errorName), the runtime's own word
// for what went wrong. A log line can carry anything the runtime decided to
// print, so the discipline that kept stderr discarded before (process.go) is
// kept by exposing a single identifier rather than the text.
//
// This is the runtime's account of itself, which session-abstraction §5.8
// allows; the driver does not classify the error, it relays the name.
type logCapture struct {
	now func() time.Time

	mu      sync.Mutex
	partial []byte
	lines   []logLine
}

type logLine struct {
	at   time.Time
	text string
}

const (
	// logKeepLines bounds the tail. A turn's failure is written within a few
	// lines of when it happens, and State reads it back soon after, so a short
	// tail is enough and a long one is just memory.
	logKeepLines = 200
	// logMaxLine bounds one retained line. The rest of an over-long line is
	// dropped: the error name this exists for sits near its start.
	logMaxLine = 2048
	// logNameLookback is how far before a failure a log line may have been
	// written and still be attributed to it.
	logNameLookback = 3 * time.Second
)

func newLogCapture() *logCapture { return &logCapture{now: time.Now} }

// Write implements io.Writer for the child's stderr. It never fails and never
// blocks the child on anything but its own short critical section.
func (l *logCapture) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.now()
	for _, b := range p {
		if b == '\n' {
			l.pushLocked(at)
			continue
		}
		if len(l.partial) < logMaxLine {
			l.partial = append(l.partial, b)
		}
	}
	return len(p), nil
}

func (l *logCapture) pushLocked(at time.Time) {
	if len(l.partial) == 0 {
		return
	}
	l.lines = append(l.lines, logLine{at: at, text: string(l.partial)})
	l.partial = l.partial[:0]
	if len(l.lines) > logKeepLines {
		l.lines = append([]logLine(nil), l.lines[len(l.lines)-logKeepLines:]...)
	}
}

// errorNameRE finds a runtime error identifier: a capitalised word ending in
// "Error", the shape of every error name the runtime's API documents
// (ProviderModelNotFoundError, APIError, MessageAbortedError, ...).
var errorNameRE = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]*Error\b`)

// genericErrorNames are names that say nothing: the very placeholder this
// capture exists to see past.
var genericErrorNames = map[string]bool{"UnknownError": true}

// errorName returns the most recent specific error name the runtime logged at
// or after since-logNameLookback, or "" when there is none. A line that names
// only the generic placeholder does not count.
func (l *logCapture) errorName(since time.Time) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := since.Add(-logNameLookback)
	for i := len(l.lines) - 1; i >= 0; i-- {
		if l.lines[i].at.Before(cutoff) {
			break
		}
		for _, name := range errorNameRE.FindAllString(l.lines[i].text, -1) {
			if !genericErrorNames[name] {
				return name
			}
		}
	}
	return ""
}

// waitErrorName is errorName, retried for up to within. The runtime writes the
// log line and publishes the failure on its bus independently, so the second
// can arrive first; a short wait closes that race without making a failure
// that logged nothing wait forever.
func (l *logCapture) waitErrorName(ctx context.Context, since time.Time, within time.Duration) string {
	if l == nil {
		return ""
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if name := l.errorName(since); name != "" {
			return name
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return l.errorName(since)
		case <-tick.C:
		}
	}
}
