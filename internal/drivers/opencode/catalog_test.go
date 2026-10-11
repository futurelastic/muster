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

// A runtime that gives no usable answer is "could not tell", never "wrong" and
// never "nothing": the create proceeds exactly as it did before the check.
func TestCreate_NoUsableCatalogProceedsUnchecked(t *testing.T) {
	cases := map[string]func(f *fakeServer){
		"endpoint absent": func(f *fakeServer) {},
		"endpoint errors": func(f *fakeServer) { f.catalogStatus = 500 },
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

// muster #293: a catalog the runtime answered, and that lists nothing, is not
// "could not tell": no provider is usable, so a session started there could only
// fall back to something nobody chose.
func TestCreate_EmptyCatalogRefusedWithAReason_NoSessionLeftBehind(t *testing.T) {
	for name, model := range map[string]string{"no model": "", "a model": "anthropic/anything"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeServer(t)
			f.catalog = map[string][]string{}
			d := newTestDriver(t, f)
			_, err := createWithModel(t, d, model)
			var fe *fleet.Error
			if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
				t.Fatalf("err = %v, want an unsupported fleet error", err)
			}
			for _, want := range []string{"no model provider is usable", `"opencode"`, "no provider at all"} {
				if !strings.Contains(fe.Message, want) {
					t.Errorf("message %q does not say %q", fe.Message, want)
				}
			}
			if len(f.sessions) != 0 {
				t.Errorf("sessions = %d, want none left behind", len(f.sessions))
			}
			for _, r := range f.requests {
				if r.method == "POST" && r.path == "/session" {
					t.Error("the runtime was asked to create a session with no usable provider")
				}
			}
		})
	}
}

// The measured case: no credential, so the only provider the runtime lists is
// its own hosted one. The default refuses it and says which provider it skipped.
func TestCreate_OnlyTheBuiltInProviderIsRefusedByDefault(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"big-pickle"}}
	d := newTestDriver(t, f)

	_, err := createWithModel(t, d, "")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want an unsupported fleet error", err)
	}
	if !strings.Contains(fe.Message, "it lists only \"opencode\"") {
		t.Errorf("message %q does not name the provider it skipped", fe.Message)
	}
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none", len(f.sessions))
	}
}

// Explicitly naming the built-in provider is how an operator asks for it.
func TestCreate_ExplicitAllowOfTheBuiltInProviderCreates(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"big-pickle"}}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"opencode"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createWithModel(t, d, "opencode/big-pickle"); err != nil {
		t.Fatalf("Create on an explicitly allowed built-in provider: %v", err)
	}
}

// With a credential, the runtime lists its provider too, and the create goes
// through; the built-in one being listed beside it does not matter.
func TestCreate_ACredentialedProviderCreatesUnderTheDefault(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"big-pickle"}, "anthropic": {"claude-sonnet-4-5"}}
	d := newTestDriver(t, f)
	if _, err := createWithModel(t, d, "anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := createWithModel(t, d, ""); err != nil {
		t.Fatalf("Create with no model: %v", err)
	}
}

// A model on the built-in provider is refused at once under the default — the
// runtime is not even asked, because nothing was started for it.
func TestCreate_ModelOnTheBuiltInProviderIsRefusedBeforeAnythingStarts(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"big-pickle"}, "anthropic": {"x"}}
	d := newTestDriver(t, f)

	_, err := createWithModel(t, d, "opencode/big-pickle")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want an unsupported fleet error", err)
	}
	if !strings.Contains(fe.Message, `provider "opencode"`) || !strings.Contains(fe.Message, "does not allow") {
		t.Errorf("message %q does not name the provider and the refusal", fe.Message)
	}
	if n := len(f.requestsSnapshot()); n != 0 {
		t.Errorf("the runtime saw %d requests for a refusal decided before it was asked", n)
	}
}

// The allow-list is the whole set: a provider the runtime lists that is not on
// it is not usable, credentialed or not.
func TestCreate_AllowListExcludesEveryOtherProvider(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"openai": {"gpt-5"}}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"anthropic"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = createWithModel(t, d, "")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want an unsupported fleet error", err)
	}
	if !strings.Contains(fe.Message, `only "anthropic"`) || !strings.Contains(fe.Message, `"openai"`) {
		t.Errorf("message %q does not name the allowed set and what the runtime listed", fe.Message)
	}
	if _, err := createWithModel(t, d, "openai/gpt-5"); err == nil {
		t.Error("a model on a provider outside the allow-list was accepted")
	}
}

// The catalog is read for every create, with or without a model: that read is
// what finds "nothing usable".
func TestCreate_NoModelStillReadsTheCatalog(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"anthropic": {"x"}}
	d := newTestDriver(t, f)
	createOne(t, d, "/work/x", "key-1")
	read := false
	for _, r := range f.requestsSnapshot() {
		if r.path == "/config/providers" {
			read = true
		}
	}
	if !read {
		t.Error("the catalog was not read for a create that named no model")
	}
}

func TestWithProviders_RefusesAnIdThatIsAModel(t *testing.T) {
	f := newFakeServer(t)
	_, err := newDriverWithOptions(t, f, WithProviders([]string{"anthropic/claude-sonnet-4-5"}))
	if err == nil || !strings.Contains(err.Error(), "not a provider id") {
		t.Fatalf("New = %v, want a refusal naming the bad id", err)
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
