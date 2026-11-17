package lowprio

import (
	"runtime"
	"testing"
	"time"
)

// A function handed to Go runs, exactly once, on its own goroutine — and where the
// platform has a background priority it runs at it.
func TestGo_RunsTheFunctionAtBackgroundPriorityWhereThereIsOne(t *testing.T) {
	type result struct {
		low bool
		err error
	}
	got := make(chan result, 1)
	Go(func() {
		low, err := current()
		got <- result{low, err}
	})
	select {
	case r := <-got:
		if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
			if r.err != nil || !r.low {
				t.Errorf("the function ran at normal priority (low=%v, err=%v), want the background class", r.low, r.err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the function never ran")
	}
}

// The lowered thread goes with its goroutine. If it went back to the pool, some
// unrelated goroutine would one day run at background priority and nothing would
// say why.
func TestGo_DoesNotLeaveALoweredThreadBehind(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no background priority on this platform")
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		Go(func() { done <- struct{}{} })
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// Whatever thread this goroutine is on now, it must be at normal priority;
	// pinned, so the answer is about one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for i := 0; i < 200; i++ {
		if low, err := current(); err != nil || low {
			t.Fatalf("a goroutine that never asked runs at background priority (low=%v, err=%v)", low, err)
		}
		runtime.Gosched()
	}
}
