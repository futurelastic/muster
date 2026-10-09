package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func createWithModel(t *testing.T, d *Driver, model string) (fleet.Session, error) {
	t.Helper()
	return d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{}), "key-"+model,
		fleet.SessionSpec{Cwd: "/work/x", Model: model})
}

func TestCreate_KnownModelCreates(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"anthropic": {"claude-sonnet-4-5", "claude-opus-4-1"}}
	d := newTestDriver(t, f)

	if _, err := createWithModel(t, d, "anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatalf("Create with a catalogued model: %v", err)
	}
	if len(f.sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(f.sessions))
	}
}

func TestCreate_UnknownModelRefusedInvalid_NoSessionLeftBehind(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"anthropic": {"claude-sonnet-4-5", "claude-opus-4-1"}}
	d := newTestDriver(t, f)

	// The near-identical spelling another harness uses for the same model.
	_, err := createWithModel(t, d, "anthropic/claude-sonnet-4.5")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
		t.Fatalf("err = %v, want an invalid fleet error", err)
	}
	if !strings.Contains(fe.Message, "anthropic/claude-sonnet-4.5") {
		t.Errorf("message %q does not name the id", fe.Message)
	}
	if !strings.Contains(fe.Message, `"anthropic/claude-sonnet-4-5"`) {
		t.Errorf("message %q does not offer the closest valid id", fe.Message)
	}
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none left behind", len(f.sessions))
	}
	for _, r := range f.requests {
		if r.method == "POST" && r.path == "/session" {
			t.Error("the runtime was asked to create a session for a refused model")
		}
	}
}

func TestCreate_UnknownModelNothingNearNamesNoSuggestion(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"anthropic": {"claude-sonnet-4-5"}}
	d := newTestDriver(t, f)

	_, err := createWithModel(t, d, "openai/gpt-9000-turbo-ultra")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
		t.Fatalf("err = %v, want invalid", err)
	}
	if strings.Contains(fe.Message, "closest") {
		t.Errorf("message %q suggests something for a model nothing resembles", fe.Message)
	}
}

// A runtime that gives no usable catalog is "could not tell", never "wrong".
func TestCreate_NoUsableCatalogProceedsUnchecked(t *testing.T) {
	cases := map[string]func(f *fakeServer){
		"endpoint absent": func(f *fakeServer) {},
		"endpoint errors": func(f *fakeServer) { f.catalogStatus = 500 },
		"lists nothing":   func(f *fakeServer) { f.catalog = map[string][]string{} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			setup(f)
			d := newTestDriver(t, f)
			if _, err := createWithModel(t, d, "anthropic/anything"); err != nil {
				t.Fatalf("Create: %v, want today's unchecked behaviour", err)
			}
		})
	}
}

func TestCreate_NoModelNeverReadsTheCatalog(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"anthropic": {"x"}}
	d := newTestDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	for _, r := range f.requests {
		if r.path == "/config/providers" {
			t.Error("the catalog was read for a create that named no model")
		}
	}
}

func TestCatalogIsReadPerCreate(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"p": {"a"}}
	d := newTestDriver(t, f)
	if _, err := createWithModel(t, d, "p/b"); err == nil {
		t.Fatal("p/b accepted before it is listed")
	}
	f.mu.Lock()
	f.catalog = map[string][]string{"p": {"a", "b"}}
	f.mu.Unlock()
	if _, err := createWithModel(t, d, "p/b"); err != nil {
		t.Fatalf("p/b refused after the runtime lists it: %v (a compiled-in or cached list)", err)
	}
}
