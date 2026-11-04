package opencode

import (
	"context"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// muster #400: the driver reports the models a create would accept, from the
// last catalog read, through the same filter the create-time check uses.

// createAgain is createWithModel under a key of its own, so a second create is a
// second create and not the first one replayed from the idempotency record.
func createAgain(t *testing.T, d *Driver, key, model string) (fleet.Session, error) {
	t.Helper()
	return d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{}), key,
		fleet.SessionSpec{Cwd: "/work/x", Model: model})
}

func idsOf(m fleet.RuntimeModels) map[string]bool {
	out := map[string]bool{}
	for _, id := range m.IDs {
		out[id] = true
	}
	return out
}

// Nothing has been read before the first create, and that is "none" — not an
// empty list the runtime vouched for.
func TestModels_NoneBeforeAnyCatalogRead(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"deepseek": {"deepseek-flash"}}
	d := newTestDriver(t, f)

	got := d.Models()
	if got.Source != fleet.ModelsNone || got.ReadAt != nil || got.Default != nil || len(got.IDs) != 0 || got.IDs == nil {
		t.Errorf("before any create: %+v, want none, no readAt, no default, an empty (non-null) list", got)
	}
	if n := len(f.requestsSnapshot()); n != 0 {
		// Models() builds the answer from memory; it must never be what reads the catalog.
		for _, r := range f.requestsSnapshot() {
			if r.path == "/config/providers" {
				t.Errorf("Models() asked the runtime for its catalog (%d requests)", n)
			}
		}
	}
}

// The invariant #400 states: the ids listed are exactly what a create accepts.
// One catalog, one policy, and every id in the raw catalog either appears in the
// list and creates, or appears in neither.
func TestModels_ListIsExactlyWhatACreateAccepts(t *testing.T) {
	catalog := map[string][]string{
		"deepseek": {"deepseek-flash", "deepseek-v4-pro"},
		"opencode": {"paid-x", "big-free"},
		"other":    {"model-y"},
	}
	// The default policy leaves the built-in provider off; a policy naming
	// providers leaves only those.
	for name, opts := range map[string][]Option{
		"default policy":   nil,
		"named providers":  {WithProviders([]string{"deepseek", "opencode"})},
		"one named, alone": {WithProviders([]string{"deepseek"})},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			f.catalog = catalog
			d, err := newDriverWithOptions(t, f, opts...)
			if err != nil {
				t.Fatal(err)
			}
			// One create reads the catalog and fills the list.
			if _, err := createWithModel(t, d, ""); err != nil {
				t.Fatalf("the filling create: %v", err)
			}
			got := d.Models()
			if got.Source != fleet.ModelsFromRuntime || got.ReadAt == nil {
				t.Fatalf("after a create: %+v, want a runtime list with a readAt", got)
			}
			listed := idsOf(got)
			for provider, models := range catalog {
				for _, m := range models {
					id := provider + "/" + m
					_, err := createWithModel(t, d, id)
					if accepted := err == nil; accepted != listed[id] {
						t.Errorf("%s: a create accepts it = %v (err %v), the list has it = %v — the list must be exactly what a create accepts",
							id, accepted, err, listed[id])
					}
				}
			}
		})
	}
}

func TestModels_DefaultIsOnlyWhatTheRuntimeSaysWithCertainty(t *testing.T) {
	cases := map[string]struct {
		catalog  map[string][]string
		defaults map[string]string
		opts     []Option
		want     string
	}{
		"one provider with a default": {
			catalog:  map[string][]string{"deepseek": {"deepseek-flash", "deepseek-v4-pro"}},
			defaults: map[string]string{"deepseek": "deepseek-flash"},
			want:     "deepseek/deepseek-flash",
		},
		"one provider, no default named": {
			catalog: map[string][]string{"deepseek": {"deepseek-flash"}},
			want:    "",
		},
		"two providers: the runtime's pick depends on an order not visible here": {
			catalog:  map[string][]string{"deepseek": {"deepseek-flash"}, "other": {"model-y"}},
			defaults: map[string]string{"deepseek": "deepseek-flash", "other": "model-y"},
			opts:     []Option{WithProviders([]string{"deepseek", "other"})},
			want:     "",
		},
		"a default that is not in the list": {
			catalog:  map[string][]string{"deepseek": {"deepseek-flash"}},
			defaults: map[string]string{"deepseek": "deepseek-gone"},
			want:     "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			f.catalog, f.catalogDefaults = tc.catalog, tc.defaults
			d, err := newDriverWithOptions(t, f, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := createWithModel(t, d, ""); err != nil {
				t.Fatalf("create: %v", err)
			}
			got := d.Models()
			switch {
			case tc.want == "" && got.Default != nil:
				t.Errorf("default = %q, want null", *got.Default)
			case tc.want != "" && (got.Default == nil || *got.Default != tc.want):
				t.Errorf("default = %v, want %q", got.Default, tc.want)
			}
			if got.Default != nil && !idsOf(got)[*got.Default] {
				t.Errorf("default %q is not among the listed ids %v", *got.Default, got.IDs)
			}
		})
	}
}

// A read that gave no usable answer leaves the list as it was: "could not tell"
// is not a reason to forget what the runtime last said.
func TestModels_AnUnusableReadKeepsTheLastAnswer(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"deepseek": {"deepseek-flash"}}
	d := newTestDriver(t, f)
	if _, err := createWithModel(t, d, ""); err != nil {
		t.Fatal(err)
	}
	first := d.Models()

	f.mu.Lock()
	f.catalogStatus = 500
	f.mu.Unlock()
	if _, err := createAgain(t, d, "second", ""); err != nil {
		t.Fatalf("an unusable catalog proceeds as it did before the check: %v", err)
	}

	again := d.Models()
	if again.Source != fleet.ModelsFromRuntime || len(again.IDs) != 1 || !again.ReadAt.Equal(*first.ReadAt) {
		t.Errorf("after an unusable read: %+v, want the first answer unchanged (%+v)", again, first)
	}
}

// The list is the LATEST answer, never a union: a settle-window re-read that
// no longer lists a model must drop it, because a create now refuses it.
func TestModels_IsTheLatestAnswerNotAUnion(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"deepseek": {"old-model"}}
	d := newTestDriver(t, f)
	if _, err := createWithModel(t, d, ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.catalog = map[string][]string{"deepseek": {"new-model"}}
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // the clock moves between reads
	if _, err := createAgain(t, d, "second", ""); err != nil {
		t.Fatal(err)
	}
	got := idsOf(d.Models())
	if got["deepseek/old-model"] || !got["deepseek/new-model"] {
		t.Errorf("list = %v, want only the latest answer's ids", d.Models().IDs)
	}
}

// An answer that listed nothing is a real answer: the runtime vouched that no
// provider here has a usable model, so the list is empty and says "runtime".
func TestModels_AnEmptyAnswerIsAnEmptyRuntimeList(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{}
	d := newTestDriver(t, f)
	_, _ = createWithModel(t, d, "") // refused, but it read the catalog
	got := d.Models()
	if got.Source != fleet.ModelsFromRuntime || len(got.IDs) != 0 || got.IDs == nil || got.ReadAt == nil {
		t.Errorf("after an empty answer: %+v, want a runtime list that is empty", got)
	}
}
