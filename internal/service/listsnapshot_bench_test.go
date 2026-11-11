package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// syntheticFleet builds n synthetic sessions: no real content, one row shape,
// fixed timestamps so two builds of the same fleet encode to the same bytes.
// Each row carries a conversation record and a long-ish evidence string so its
// encoded size is of the order a real row has (a couple of kilobytes) rather
// than the few hundred bytes of a bare ref.
func syntheticFleet(n int) []fleet.Session {
	started := fleet.Timestamp(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	out := make([]fleet.Session, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fleet.Session{
			SessionRef: fleet.SessionRef{Machine: "testbox", ID: fmt.Sprintf("syn-%04d", i)},
			Runtime:    "fake",
			Cwd:        fleet.AbsolutePath(fmt.Sprintf("/work/synthetic/%03d", i)),
			StartedAt:  &started,
			State: fleet.InferredState(fleet.StatusIdle,
				"synthetic evidence line, repeated to give the row a realistic weight: waiting at the prompt, nothing running, last output unchanged since the previous reading",
				nil),
		})
	}
	return out
}

// fleetFakeDriver is a driver that can both list a table of synthetic sessions,
// counting the enumerations, and stream — so the service treats it as watched
// and a test can arm the event plane against it.
type fleetFakeDriver struct {
	feedDriver

	tmu      sync.Mutex
	calls    int
	sessions []fleet.Session
	// source is what List reports about itself; the zero value is a clean ok.
	source *fleet.SourceStatus
	// delay makes an enumeration take a while, for the tests that need two
	// callers to overlap.
	delay time.Duration
}

func newFleetFakeDriver(sessions []fleet.Session) *fleetFakeDriver {
	return &fleetFakeDriver{feedDriver: *newFeedDriver(0, nil), sessions: sessions}
}

func (d *fleetFakeDriver) List(ctx context.Context, _ fleet.Request, f driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	d.tmu.Lock()
	d.calls++
	rows := append([]fleet.Session(nil), d.sessions...)
	src := fleet.SourceStatus{Machine: "testbox", Status: fleet.SourceOK, ObservedAt: time.Now()}
	if d.source != nil {
		src = *d.source
		src.ObservedAt = time.Now()
	}
	delay := d.delay
	d.tmu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fleet.Collection[fleet.Session]{}, ctx.Err()
		}
	}
	var out []fleet.Session
	for _, s := range rows {
		if f.ID == "" || s.ID == f.ID {
			out = append(out, s)
		}
	}
	return fleet.NewCollection(out, []fleet.SourceStatus{src})
}

// Enumerations is how many times the service asked this driver for its rows.
func (d *fleetFakeDriver) Enumerations() int {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	return d.calls
}

// Replace swaps the table, which is how a test makes "the fleet changed".
func (d *fleetFakeDriver) Replace(sessions []fleet.Session) {
	d.tmu.Lock()
	d.sessions = sessions
	d.tmu.Unlock()
}

// BenchmarkListSplit is the profile #464 asks for BEFORE choosing a fix: one
// list call, split into the three costs it is made of. The fake enumerates for
// free, so `enumerate` here is the floor (a copy of the table); what the real
// drivers add on top is the difference between a measured list and
// rows+encode.
func BenchmarkListSplit(b *testing.B) {
	for _, n := range []int{200, 500} {
		rows := syntheticFleet(n)

		b.Run(fmt.Sprintf("n=%d/enumerate", n), func(b *testing.B) {
			d := newFleetFakeDriver(rows)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := d.List(context.Background(), fleet.SystemRequest(), driver.ListFilter{}); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("n=%d/rows", n), func(b *testing.B) {
			// ListSessions less the driver's own enumeration: label lookups,
			// the closed-record bookkeeping, the cwd index and the envelope.
			svc := New("testbox")
			d := newFleetFakeDriver(rows)
			if err := svc.RegisterLocalDriver("fake", d); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := svc.ListSessions(context.Background(), fleet.SystemRequest(), ScopeLocal, driver.ListFilter{}, 0); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("n=%d/encode", n), func(b *testing.B) {
			col, err := fleet.NewCollection(rows, []fleet.SourceStatus{{Machine: "testbox", Status: fleet.SourceOK, ObservedAt: time.Now()}})
			if err != nil {
				b.Fatal(err)
			}
			col = col.WithFeed(1, "e")
			var buf bytes.Buffer
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Reset()
				if err := json.NewEncoder(&buf).Encode(col); err != nil {
					b.Fatal(err)
				}
			}
			b.SetBytes(int64(buf.Len()))
		})
	}
}

// BenchmarkListHTTP is the whole request through handleListSessions: the cost
// a caller polling the list pays per read. `cold` changes something between
// reads (so every read rebuilds); `warm` reads an unchanged fleet.
func BenchmarkListHTTP(b *testing.B) {
	for _, n := range []int{200, 500} {
		rows := syntheticFleet(n)
		newServiceFor := func(b *testing.B) (*Service, http.Handler, *fleetFakeDriver) {
			svc := New("testbox")
			d := newFleetFakeDriver(rows)
			if err := svc.RegisterLocalDriver("fake", d); err != nil {
				b.Fatal(err)
			}
			// Arm the event plane the way a watching client does, so the
			// service has a live stream to be coherent against.
			_, _, _, cancel := svc.Events(context.Background(), ScopeLocal, driver.SubscribeFilter{}, 0, "")
			b.Cleanup(cancel)
			deadline := time.Now().Add(3 * time.Second)
			for !svc.events.streamLive() && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			// Let the pump subscribe: a stream that has not yet attached is a
			// hole, and the service rightly refuses to rely on it.
			for d.Attempts() < 1 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			return svc, NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}), d
		}
		get := func(h http.Handler) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions?scope=local", nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}

		b.Run(fmt.Sprintf("n=%d/cold", n), func(b *testing.B) {
			svc, h, _ := newServiceFor(b)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Any hub event is a change as far as a snapshot can tell.
				svc.events.publish(stateEventOn("testbox", fleet.EventSessionState, "syn-0000"))
				if rec := get(h); rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})

		b.Run(fmt.Sprintf("n=%d/warm", n), func(b *testing.B) {
			_, h, _ := newServiceFor(b)
			get(h) // the one build every later read is served from
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if rec := get(h); rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}
