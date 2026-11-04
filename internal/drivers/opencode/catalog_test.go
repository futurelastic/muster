package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// A provider entry that cannot be one stops the driver, naming the entry. Until
// muster #401 any "provider/model" was refused here; it is now a model rule, so
// the malformed shapes are the ones the grammar (provider_test.go) lists.
func TestWithProviders_RefusesAMalformedEntryNamingIt(t *testing.T) {
	f := newFakeServer(t)
	_, err := newDriverWithOptions(t, f, WithProviders([]string{"anthropic", "opencode/"}))
	if err == nil || !strings.Contains(err.Error(), `"opencode/"`) || !strings.Contains(err.Error(), "provider policy entry") {
		t.Fatalf("New = %v, want a refusal naming the offending entry", err)
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

// muster #384: a freshly started server answers with the model list built into
// the binary until it has downloaded a newer one, and for one provider the two
// lists share no id. The answers below are the two lists measured on a real
// runtime, asked about the same provider.
var (
	builtInModels  = map[string][]string{"deepseek": {"deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}}
	refreshedModel = map[string][]string{"deepseek": {"deepseek-flash", "deepseek-v4-pro"}}
)

func providerReads(f *fakeServer) int {
	n := 0
	for _, r := range f.requestsSnapshot() {
		if r.method == "GET" && r.path == "/config/providers" {
			n++
		}
	}
	return n
}

func TestCreate_ModelOnlyInTheRefreshedListIsNotRefusedForAnsweringBeforeTheRefresh(t *testing.T) {
	f := newFakeServer(t)
	f.catalogSeq = []map[string][]string{builtInModels, refreshedModel}
	d := newTestDriver(t, f)

	if _, err := createWithModel(t, d, "deepseek/deepseek-flash"); err != nil {
		t.Fatalf("a model in the refreshed list was refused because the first answer was the built-in one: %v", err)
	}
	if got := providerReads(f); got != 2 {
		t.Errorf("catalog reads = %d, want 2 (the first answer did not list it, the second did)", got)
	}
}

func TestCreate_RefreshArrivingAfterSeveralAnswersStillCreates(t *testing.T) {
	f := newFakeServer(t)
	f.catalogSeq = []map[string][]string{builtInModels, builtInModels, builtInModels, refreshedModel}
	d := newTestDriver(t, f)

	if _, err := createWithModel(t, d, "deepseek/deepseek-v4-pro"); err != nil {
		t.Fatalf("a model listed by the fourth answer was refused: %v", err)
	}
}

// Either list is valid: a model the first answer lists is accepted on it, with
// no wait and no second read.
func TestCreate_ModelInTheBuiltInListCreatesOnTheFirstAnswer(t *testing.T) {
	f := newFakeServer(t)
	f.catalogSeq = []map[string][]string{builtInModels, refreshedModel}
	d := newTestDriver(t, f)

	if _, err := createWithModel(t, d, "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("a model in the built-in list was refused: %v", err)
	}
	if got := providerReads(f); got != 1 {
		t.Errorf("catalog reads = %d, want 1 (a listed model must not wait for a second answer)", got)
	}
}

// #287's refusal stays for an id that no answer lists, and it suggests from
// every answer it saw: each suggestion is one the check would accept.
func TestCreate_ModelInNeitherListIsStillRefused_SuggestingFromBothAnswers(t *testing.T) {
	f := newFakeServer(t)
	f.catalogSeq = []map[string][]string{builtInModels, refreshedModel}
	d := newTestDriver(t, f)

	_, err := createWithModel(t, d, "deepseek/deepseek-flsh")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
		t.Fatalf("err = %v, want an invalid fleet error", err)
	}
	for _, want := range []string{`"deepseek/deepseek-flash"`, `"deepseek/deepseek-v4-flash"`} {
		if !strings.Contains(fe.Message, want) {
			t.Errorf("message %q does not offer %s", fe.Message, want)
		}
	}
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none left behind", len(f.sessions))
	}
	if got := providerReads(f); got < 2 {
		t.Errorf("catalog reads = %d, want the list re-read before refusing", got)
	}
}

// Waiting for the list is bounded: a catalog that never gains the model ends in
// the refusal, after the window and not before.
func TestCreate_UnlistedModelIsRefusedOnceTheWindowEnds(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"p": {"a"}}
	d := newTestDriver(t, f)
	d.catalogSettle, d.catalogPoll = 60*time.Millisecond, 10*time.Millisecond

	start := time.Now()
	_, err := createWithModel(t, d, "p/b")
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorInvalid {
		t.Fatalf("err = %v, want an invalid fleet error", err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("refused after %v, before the %v window ended", elapsed, d.catalogSettle)
	}
	if got := providerReads(f); got < 3 {
		t.Errorf("catalog reads = %d, want the window polled more than once", got)
	}
}

// The caller's deadline ending during the wait is a deadline, not a verdict on
// the model.
func TestCreate_DeadlineEndingDuringTheWaitIsNotAModelRefusal(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"p": {"a"}}
	d := newTestDriver(t, f)
	d.catalogSettle, d.catalogPoll = 10*time.Second, 5*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := d.Create(ctx, fleet.RequestFrom(fleet.Caller{}), "key-deadline",
		fleet.SessionSpec{Cwd: "/work/x", Model: "p/b"})
	var fe *fleet.Error
	if errors.As(err, &fe) && fe.Kind == fleet.ErrorInvalid {
		t.Fatalf("err = %v: the caller's deadline was reported as a refused model", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context deadline", err)
	}
}

// ---- model rules (muster #401) ----------------------------------------------

// refusalOf is the message of an unsupported refusal, "" when err is nil.
func refusalOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want an unsupported fleet error", err)
	}
	return fe.Message
}

// requireNothingStarted asserts a refusal cost nothing: no session on the
// runtime, and no request that would create one.
func requireNothingStarted(t *testing.T, f *fakeServer) {
	t.Helper()
	if len(f.sessions) != 0 {
		t.Errorf("sessions = %d, want none left behind", len(f.sessions))
	}
	for _, r := range f.requestsSnapshot() {
		if r.method == "POST" && r.path == "/session" {
			t.Error("the runtime was asked to create a session for a refused model")
		}
	}
}

// The policy an operator should write: whole providers, plus the EXACT models
// wanted on one that also serves models they do not want. A name pattern is not
// a reliable mark of a model's terms, so the allowlist names the models.
func TestCreate_ExactModelAllowlist(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{
		"deepseek": {"deepseek-flash", "deepseek-v4-pro"},
		// The hosted provider serves paid models, a free one with the suffix, and
		// a zero-cost one WITHOUT it.
		"opencode": {"deepseek-v4.1-flash", "deepseek-v4-pro", "big-pickle", "grok-code", "big-free"},
	}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{
		"deepseek", "opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro",
	}))
	if err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{"deepseek/deepseek-flash", "opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro"} {
		if _, err := createWithModel(t, d, model); err != nil {
			t.Errorf("a create pinned to %s: %v, want it allowed", model, err)
		}
	}
	// Every other model on the hosted provider is refused — the suffix-less
	// zero-cost ones included, which no suffix rule would have caught.
	for _, model := range []string{"opencode/big-pickle", "opencode/grok-code", "opencode/big-free"} {
		f := newFakeServer(t)
		f.catalog = map[string][]string{"deepseek": {"deepseek-flash"}, "opencode": {"big-pickle", "grok-code", "big-free", "deepseek-v4-pro"}}
		d, err := newDriverWithOptions(t, f, WithProviders([]string{"deepseek", "opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro"}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = createWithModel(t, d, model)
		msg := refusalOf(t, err)
		for _, want := range []string{model, `where this machine allows only some models and none matches it`,
			`only "deepseek", "opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: refusal %q does not say %q", model, msg, want)
			}
		}
		requireNothingStarted(t, f)
		for _, r := range f.requestsSnapshot() {
			if r.path == "/config/providers" {
				t.Errorf("%s: refused only after the runtime was asked for its catalog; the policy decides it before any process exists", model)
			}
		}
	}
	// A provider the policy never names stays refused as it always was.
	if msg := refusalOf(t, d.checkModelProvider("elsewhere/model")); !strings.Contains(msg, `which this machine does not allow`) {
		t.Errorf("a provider nobody named: %q", msg)
	}
}

// A deny glob: the models an operator knows by a stable name. It is NOT a
// free-tier filter — the zero-cost model without the suffix passes it, which is
// why the exact allowlist above is the documented way to keep those out.
func TestCreate_DenyGlobBeatsAnAllowAndIsNotAFreeTierFilter(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"paid-x", "big-free", "big-pickle"}, "deepseek": {"deepseek-flash"}}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"deepseek", "opencode", "!opencode/*-free"}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := createWithModel(t, d, "opencode/paid-x"); err != nil {
		t.Errorf("a model on the provider no deny matches: %v", err)
	}
	// The suffix-less zero-cost model is let through by a suffix deny.
	if _, err := createWithModel(t, d, "opencode/big-pickle"); err != nil {
		t.Errorf("opencode/big-pickle: %v (the deny matches by name only, and this name has no -free)", err)
	}

	msg := refusalOf(t, d.checkModelProvider("opencode/big-free"))
	if !strings.Contains(msg, `model "opencode/big-free" is denied by !opencode/*-free on this machine`) {
		t.Errorf("a model the deny matches: %q, want it to name the model and the rule", msg)
	}
	if !strings.Contains(msg, `denied: "!opencode/*-free"`) {
		t.Errorf("refusal %q does not carry the effective policy", msg)
	}
	// Through the whole create, nothing is started and the catalog is never read.
	g := newFakeServer(t)
	g.catalog = f.catalog
	e, err := newDriverWithOptions(t, g, WithProviders([]string{"deepseek", "opencode", "!opencode/*-free"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = createWithModel(t, e, "opencode/big-free")
	if got := refusalOf(t, err); !strings.Contains(got, "denied by !opencode/*-free") {
		t.Errorf("create: %q, want the deny named", got)
	}
	requireNothingStarted(t, g)
}

// Deny beats allow whichever is written first, and a deny alone narrows the
// default posture without lifting the built-in provider's default-off.
func TestCreate_DenyBeatsAllowInEitherOrder(t *testing.T) {
	for _, entries := range [][]string{
		{"opencode", "!opencode/big-*"},
		{"!opencode/big-*", "opencode"},
		{"opencode/big-pickle", "!opencode/big-*"},
	} {
		f := newFakeServer(t)
		d, err := newDriverWithOptions(t, f, WithProviders(entries))
		if err != nil {
			t.Fatal(err)
		}
		if msg := refusalOf(t, d.checkModelProvider("opencode/big-pickle")); !strings.Contains(msg, "denied by !opencode/big-*") {
			t.Errorf("%v: %q, want the deny to win", entries, msg)
		}
	}
	// A deny alone: everything but the built-in provider, minus the denied model.
	f := newFakeServer(t)
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"!deepseek/v3*"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.checkModelProvider("deepseek/v4"); err != nil {
		t.Errorf("deepseek/v4: %v, want it allowed", err)
	}
	if msg := refusalOf(t, d.checkModelProvider("deepseek/v3-old")); !strings.Contains(msg, "denied by !deepseek/v3*") {
		t.Errorf("deepseek/v3-old: %q", msg)
	}
	if msg := refusalOf(t, d.checkModelProvider("opencode/anything")); !strings.Contains(msg, "does not allow") {
		t.Errorf("a deny alone must not lift the built-in provider's default-off: %q", msg)
	}
}

// The catalog check applies the model rules too: a catalog whose only models on
// an allowed provider are outside the rules leaves nothing usable, and says why.
func TestCreate_NothingUsableWhenEveryListedModelIsOutsideTheRules(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"big-pickle", "grok-code"}}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"opencode/deepseek-v4-pro"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = createWithModel(t, d, "")
	msg := refusalOf(t, err)
	for _, want := range []string{"no model provider is usable", `it lists only "opencode"`,
		`every model it lists on "opencode" is outside the models this machine allows`} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	requireNothingStarted(t, f)
}

// ---- an unpinned create under model rules (muster #401) -----------------------

// requestsTo counts the requests the fake runtime saw for a method and path.
func requestsTo(f *fakeServer, method, path string) int {
	n := 0
	for _, r := range f.requestsSnapshot() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

// A create that pins no model lands on whatever the runtime picks. Under a
// policy that allows only some models of a provider, the pick must be known to
// be one of them, so it is learned first and judged.
func TestCreate_UnpinnedJudgesTheRuntimesDefaultAgainstTheModelRules(t *testing.T) {
	exact := []string{"opencode/deepseek-v4.1-flash", "opencode/deepseek-v4-pro"}
	denyGlob := []string{"opencode", "!opencode/*-free"}
	cases := []struct {
		name     string
		policy   []string
		catalog  map[string][]string
		defaults map[string]string
		config   map[string]any
		status   int // GET /config status, 0 = serve config / 404 when nil
		want     string
		// wantErr is a phrase the refusal must carry; "" means the create succeeds.
		wantErr string
	}{
		{
			name:     "exact allowlist, the default is on it",
			policy:   exact,
			catalog:  map[string][]string{"opencode": {"deepseek-v4-pro", "big-pickle"}},
			defaults: map[string]string{"opencode": "deepseek-v4-pro"},
			config:   map[string]any{},
		},
		{
			name:     "exact allowlist, the default is not on it",
			policy:   exact,
			catalog:  map[string][]string{"opencode": {"deepseek-v4-pro", "big-pickle"}},
			defaults: map[string]string{"opencode": "big-pickle"},
			config:   map[string]any{},
			wantErr:  `the runtime's default model "opencode/big-pickle" is on provider "opencode", where this machine allows only some models and none matches it`,
		},
		{
			name:     "deny glob, the default is denied",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x", "big-free"}},
			defaults: map[string]string{"opencode": "big-free"},
			config:   map[string]any{},
			wantErr:  `the runtime's default model "opencode/big-free" is denied by !opencode/*-free`,
		},
		{
			name:     "deny glob, the default is allowed",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x", "big-free"}},
			defaults: map[string]string{"opencode": "paid-x"},
			config:   map[string]any{},
		},
		{
			name:     "the configuration names a denied model, over an allowed catalog default",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x", "big-free"}},
			defaults: map[string]string{"opencode": "paid-x"},
			config:   map[string]any{"model": "opencode/big-free"},
			wantErr:  `the runtime's default model "opencode/big-free" is denied by !opencode/*-free`,
		},
		{
			name:     "the configuration names an allowed model, over a denied catalog default",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x", "big-free"}},
			defaults: map[string]string{"opencode": "big-free"},
			config:   map[string]any{"model": "opencode/paid-x"},
		},
		{
			name:     "two providers, both defaults allowed",
			policy:   []string{"deepseek", "opencode/paid-x"},
			catalog:  map[string][]string{"deepseek": {"deepseek-flash"}, "opencode": {"paid-x"}},
			defaults: map[string]string{"deepseek": "deepseek-flash", "opencode": "paid-x"},
			config:   map[string]any{},
		},
		{
			name:     "two providers, one default is not allowed: the runtime may pick either",
			policy:   []string{"deepseek", "opencode/paid-x"},
			catalog:  map[string][]string{"deepseek": {"deepseek-flash"}, "opencode": {"paid-x", "big-pickle"}},
			defaults: map[string]string{"deepseek": "deepseek-flash", "opencode": "big-pickle"},
			config:   map[string]any{},
			wantErr:  `the runtime's default model "opencode/big-pickle"`,
		},
		{
			name:    "a listed provider names no default",
			policy:  denyGlob,
			catalog: map[string][]string{"opencode": {"paid-x"}},
			config:  map[string]any{},
			wantErr: `it could not be learned (the runtime named no default model for "opencode")`,
		},
		{
			name:     "the configuration cannot be read",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x"}},
			defaults: map[string]string{"opencode": "paid-x"},
			status:   500,
			wantErr:  "it could not be learned (the runtime's configuration could not be read",
		},
		{
			name:     "the runtime has no configuration endpoint",
			policy:   denyGlob,
			catalog:  map[string][]string{"opencode": {"paid-x"}},
			defaults: map[string]string{"opencode": "paid-x"},
			wantErr:  "it could not be learned",
		},
		{
			name:    "the catalog cannot be read and the configuration names nothing",
			policy:  denyGlob,
			config:  map[string]any{},
			wantErr: "it could not be learned (the runtime did not list its models)",
		},
		{
			name:   "the catalog cannot be read but the configuration names an allowed model",
			policy: denyGlob,
			config: map[string]any{"model": "opencode/paid-x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServer(t)
			f.catalog, f.catalogDefaults, f.config, f.configStatus = tc.catalog, tc.defaults, tc.config, tc.status
			d, err := newDriverWithOptions(t, f, WithProviders(tc.policy))
			if err != nil {
				t.Fatal(err)
			}
			_, err = createWithModel(t, d, "")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v, want it allowed", err)
				}
				return
			}
			msg := refusalOf(t, err)
			for _, want := range []string{"create: no model was pinned", tc.wantErr, "(allowed: "} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not say %q", msg, want)
				}
			}
			requireNothingStarted(t, f)
		})
	}
}

// Only a policy that judges single models needs the runtime's default: a
// provider-only policy never asks for the configuration, and a pinned create
// has chosen its model.
func TestCreate_TheConfigurationIsReadOnlyWhereTheDefaultMatters(t *testing.T) {
	// Provider-only: today's behaviour, byte for byte, with no extra request.
	f := newFakeServer(t)
	f.catalog = map[string][]string{"deepseek": {"deepseek-flash"}}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"deepseek"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createWithModel(t, d, ""); err != nil {
		t.Fatalf("provider-only, unpinned: %v", err)
	}
	if n := requestsTo(f, "GET", "/config"); n != 0 {
		t.Errorf("a provider-only policy read GET /config %d times, want 0", n)
	}

	// Model rules, pinned to an allowed model: the pin is the answer.
	g := newFakeServer(t)
	g.catalog = map[string][]string{"opencode": {"paid-x", "big-free"}}
	e, err := newDriverWithOptions(t, g, WithProviders([]string{"opencode", "!opencode/*-free"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createWithModel(t, e, "opencode/paid-x"); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	if n := requestsTo(g, "GET", "/config"); n != 0 {
		t.Errorf("a pinned create read GET /config %d times, want 0", n)
	}
}

// The directory the session is created in is the one the configuration is read
// for: a project may configure a model of its own.
func TestCreate_TheConfiguredModelIsReadForTheSessionsDirectory(t *testing.T) {
	f := newFakeServer(t)
	f.catalog = map[string][]string{"opencode": {"paid-x"}}
	f.catalogDefaults = map[string]string{"opencode": "paid-x"}
	f.config = map[string]any{}
	d, err := newDriverWithOptions(t, f, WithProviders([]string{"opencode", "!opencode/*-free"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createWithModel(t, d, ""); err != nil {
		t.Fatal(err)
	}
	var query string
	for _, r := range f.requestsSnapshot() {
		if r.method == "GET" && r.path == "/config" {
			query = r.query
		}
	}
	if !strings.Contains(query, "directory=%2Fwork%2Fx") {
		t.Errorf("GET /config query = %q, want the session's directory", query)
	}
}
