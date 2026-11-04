package opencode

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
)

// wireProviders is the subset of the runtime's GET /config/providers this
// driver reads: each provider that is usable on this server, and the model ids
// it offers. A provider with no credential here is not listed, which is the
// point — the catalog answers "what would a first turn accept", not "what does
// the vendor sell".
type wireProviders struct {
	Providers []struct {
		ID     string              `json:"id"`
		Models map[string]struct{} `json:"models"`
	} `json:"providers"`
	// Default is the runtime's own pick per provider: provider id to model id.
	// The runtime sends it as an empty object when it has none to name.
	Default map[string]string `json:"default"`
}

// catalogAnswer is what one usable read of the runtime's catalog carried: every
// "provider/model" id it listed, before any policy, and the runtime's own
// default model per provider.
type catalogAnswer struct {
	ids      modelCatalog
	defaults map[string]string
}

// catalogSnapshot is the last usable answer this driver read, and when
// (muster #400). It is where GET /v1/runtimes gets the model list from: the
// driver makes no request of its own to build it, so there is one read of the
// catalog and one filter over it, and the list cannot disagree with what a
// create accepts.
type catalogSnapshot struct {
	answer catalogAnswer
	readAt time.Time
}

// modelCatalog is the runtime's answer flattened to "provider/model" ids, the
// spelling SessionSpec.Model uses.
type modelCatalog []string

// has reports whether id is one of the catalog's entries, exactly. The runtime
// is case-sensitive about its ids and so is this.
func (c modelCatalog) has(id string) bool {
	for _, have := range c {
		if have == id {
			return true
		}
	}
	return false
}

// union returns the entries of both, each once, sorted.
func (c modelCatalog) union(other modelCatalog) modelCatalog {
	out := append(modelCatalog(nil), c...)
	for _, id := range other {
		if !out.has(id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// closest returns up to n catalog ids nearest to want, nearest first, and none
// when nothing is near enough to be a plausible misspelling — a wrong
// suggestion is worse than none.
func (c modelCatalog) closest(want string, n int) []string {
	type scored struct {
		id   string
		dist int
	}
	norm := normalizeModelID(want)
	limit := len(norm)/3 + 1
	var near []scored
	for _, have := range c {
		d := editDistance(norm, normalizeModelID(have))
		if d <= limit {
			near = append(near, scored{have, d})
		}
	}
	sort.SliceStable(near, func(i, j int) bool {
		if near[i].dist != near[j].dist {
			return near[i].dist < near[j].dist
		}
		return near[i].id < near[j].id
	})
	if len(near) > n {
		near = near[:n]
	}
	out := make([]string, len(near))
	for i, s := range near {
		out[i] = s.id
	}
	return out
}

// normalizeModelID folds the differences between spellings of one model that
// runtimes disagree on: case, and which of "-", "." and "_" separates a
// version.
func normalizeModelID(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '_':
			return '-'
		}
		return r
	}, strings.ToLower(s))
}

// editDistance is the Levenshtein distance between two strings, over bytes.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// catalogRead is what asking the runtime for its catalog came to.
type catalogRead int

const (
	// catalogUnknown: the runtime gave no usable answer — the endpoint is absent
	// (an older runtime), it answered an error, or its body is not the shape
	// this driver reads. That is "this driver could not tell", never "none".
	catalogUnknown catalogRead = iota
	// catalogListed: at least one model was listed.
	catalogListed
	// catalogEmpty: the runtime answered, in the shape this driver reads, and
	// listed no provider with a model. No provider is usable on this server.
	catalogEmpty
)

// readCatalog asks the session's own server which models it would accept.
//
// The answer keeps "could not tell" (catalogUnknown) apart from "listed
// nothing" (catalogEmpty), which the first version of this check folded
// together. Unknown proceeds exactly as it did before the check existed (§5.7:
// absence of evidence is not evidence of absence). Empty is a positive answer —
// the runtime says no provider here has a usable model — and muster #293 refuses
// a create on it: a session started there can only fall back to something the
// operator did not choose. "Listed nothing" requires the answer to carry the
// providers list, so a body of some other shape is still unknown.
//
// A server that does not answer at all is an error, because the create would
// fail on it anyway.
func (d *Driver) readCatalog(ctx context.Context, srv *server) (ans catalogAnswer, read catalogRead, err error) {
	var wire wireProviders
	if err := d.do(ctx, srv, "GET", "/config/providers", nil, &wire); err != nil {
		if isUnreachable(err) {
			return catalogAnswer{}, catalogUnknown, err
		}
		return catalogAnswer{}, catalogUnknown, nil
	}
	if wire.Providers == nil {
		return catalogAnswer{}, catalogUnknown, nil
	}
	for _, p := range wire.Providers {
		for id := range p.Models {
			ans.ids = append(ans.ids, p.ID+"/"+id)
		}
	}
	sort.Strings(ans.ids)
	ans.defaults = wire.Default
	// Every usable answer is the freshest the machine has, so it is what the
	// runtime list reports (muster #400) — including each re-read in the
	// settle window below, which is why the snapshot is the latest answer and
	// never the union of several.
	d.noteCatalog(ans)
	if len(ans.ids) == 0 {
		return ans, catalogEmpty, nil
	}
	return ans, catalogListed, nil
}

// noteCatalog records ans as the latest usable answer.
func (d *Driver) noteCatalog(ans catalogAnswer) {
	snap := &catalogSnapshot{answer: catalogAnswer{
		ids:      append(modelCatalog(nil), ans.ids...),
		defaults: make(map[string]string, len(ans.defaults)),
	}, readAt: d.now().UTC()}
	for p, m := range ans.defaults {
		snap.answer.defaults[p] = m
	}
	d.catMu.Lock()
	defer d.catMu.Unlock()
	if d.lastCatalog != nil && snap.readAt.Before(d.lastCatalog.readAt) {
		return
	}
	d.lastCatalog = snap
}

// Models is the list GET /v1/runtimes reports for this runtime (muster #400):
// the ids a create on this machine would accept, from the last catalog read.
// Before any create has read one there is nothing to report, and that is
// "none", not an empty list the runtime vouched for.
//
// It is built by within, the same filter checkProviders and settleModel use, so
// the list is exactly the catalog entries the operator's provider policy leaves
// usable. It makes no request.
func (d *Driver) Models() fleet.RuntimeModels {
	d.catMu.Lock()
	snap := d.lastCatalog
	d.catMu.Unlock()
	if snap == nil {
		return fleet.NoModels()
	}
	ids := snap.answer.ids.within(d.providers)
	readAt := snap.readAt
	m := fleet.RuntimeModels{
		Source: fleet.ModelsFromRuntime,
		ReadAt: &readAt,
		IDs:    []string(ids),
	}
	if def := snap.answer.soleDefault(ids); def != "" {
		m.Default = &def
	}
	return m.Normalized()
}

var _ driver.ModelsReporter = (*Driver)(nil)

// soleDefault is the model the runtime would use for a create that names none,
// when this answer says so with certainty: exactly one provider lists models,
// the answer names a default for it, and that model is among usable. With
// several providers the runtime's pick depends on an order this driver has no
// way to see, and a guess is worse than null.
func (a catalogAnswer) soleDefault(usable modelCatalog) string {
	providers := map[string]bool{}
	for _, id := range a.ids {
		providers[providerOfModel(id)] = true
	}
	if len(providers) != 1 {
		return ""
	}
	for p := range providers {
		def, ok := a.defaults[p]
		if !ok || def == "" {
			return ""
		}
		if id := p + "/" + def; usable.has(id) {
			return id
		}
	}
	return ""
}

// within returns the entries the policy permits: those whose provider it allows
// and, where it names models, whose model it allows and no deny matches. It is
// the one place "is this id usable on this machine" is decided: checkProviders
// and settleModel use it to accept a create, and Models uses it to list what a
// create would accept (muster #400), so the two cannot drift.
func (c modelCatalog) within(p providerPolicy) modelCatalog {
	var out modelCatalog
	for _, id := range c {
		if p.permitsModel(id) {
			out = append(out, id)
		}
	}
	return out
}

// checkProviders refuses a create the provider policy leaves nothing to run on,
// and one whose model the runtime does not list. Called with the session's own
// server up and before any session exists on it. The catalog is read from the
// runtime on every create, never compiled in: catalogs change with runtime
// releases and with the credentials a session was given.
//
// model may be empty: the session then runs on whatever the runtime picks from
// what is usable, which is exactly why "nothing usable" has to be a refusal.
//
// A model the first answer does not list is not refused on that answer alone
// (muster #384). A freshly started server answers with the model list built
// into the binary until it has downloaded a newer one, and the two lists can
// disagree about a provider so that no id is in both; a refusal on the first
// answer would then refuse whatever an operator configured, some of the time.
// So the list is read again, for up to catalogSettle, and the model is accepted
// if any answer lists it. Only an id that no answer in the window listed is
// refused, with the nearest ids from every answer it saw — each of which this
// check would accept.
func (d *Driver) checkProviders(ctx context.Context, srv *server, model string) error {
	answer, read, err := d.readCatalog(ctx, srv)
	if err != nil {
		return err
	}
	cat := answer.ids
	switch read {
	case catalogUnknown:
		return nil
	case catalogEmpty:
		return d.noUsableProvider(nil)
	}
	usable := cat.within(d.providers)
	if len(usable) == 0 {
		return d.noUsableProvider(cat)
	}
	if model == "" || usable.has(model) {
		return nil
	}
	seen, err := d.settleModel(ctx, srv, model, usable)
	if err != nil {
		return err
	}
	if seen.has(model) {
		return nil
	}
	msg := fmt.Sprintf("create: model %q is not in the runtime's own catalog", model)
	if near := seen.closest(model, 3); len(near) > 0 {
		msg += "; closest: " + strings.Join(quoteAll(near), ", ")
	}
	return &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg, Machine: d.machine}
}

// settleModel asks the runtime for its catalog again, every catalogPoll for up
// to catalogSettle, until an answer lists model. first is what the answer that
// did not list it contained (already within the provider policy). It returns
// every id any answer listed, so the caller can both accept a model one of them
// listed and suggest from all of them.
//
// An answer that is not a listing (the endpoint failed, or listed nothing) adds
// nothing and is waited out like any other; the one error it returns is a server
// that stopped answering, which the create would fail on anyway, or the
// caller's own context ending, which must not read as a model refusal.
func (d *Driver) settleModel(ctx context.Context, srv *server, model string, first modelCatalog) (modelCatalog, error) {
	seen := first
	window := time.NewTimer(d.catalogSettle)
	defer window.Stop()
	tick := time.NewTicker(d.catalogPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-window.C:
			return seen, nil
		case <-tick.C:
		}
		answer, read, err := d.readCatalog(ctx, srv)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if read != catalogListed {
			continue
		}
		seen = seen.union(answer.ids.within(d.providers))
		if seen.has(model) {
			return seen, nil
		}
	}
}

// noUsableProvider is the refusal for a runtime that offers nothing the policy
// allows. listed is what the runtime did list (empty when it listed nothing), so
// the reason can say which providers were skipped.
func (d *Driver) noUsableProvider(listed modelCatalog) error {
	msg := "create: no model provider is usable: the runtime lists none that this machine allows (allowed: " +
		d.providers.describe() + ")"
	if len(listed) > 0 {
		usable := map[string]bool{}
		for _, id := range listed.within(d.providers) {
			usable[providerOfModel(id)] = true
		}
		seen := map[string]bool{}
		var skipped, narrowed []string
		for _, id := range listed {
			p := providerOfModel(id)
			if seen[p] {
				continue
			}
			seen[p] = true
			skipped = append(skipped, p)
			// A provider the policy allows, whose listed models the policy's model
			// rules all leave out: not skipped for its provider, and the reason
			// reads differently.
			if d.providers.permits(p) && !usable[p] {
				narrowed = append(narrowed, p)
			}
		}
		msg += "; it lists only " + strings.Join(quoteAll(skipped), ", ")
		if len(narrowed) > 0 {
			msg += `; every model it lists on ` + strings.Join(quoteAll(narrowed), ", ") +
				` is outside the models this machine allows`
		}
	} else {
		msg += "; it lists no provider at all, so the session has no credential for any"
	}
	msg += ". Give the session a credential for an allowed provider, or change the operator's provider setting; " +
		"a session is never started on a provider nobody chose"
	return &fleet.Error{Kind: fleet.ErrorUnsupported, Message: msg, Machine: d.machine}
}

// checkModelProvider refuses, before anything is started, a model the policy
// does not allow: one on a provider it does not allow, one a deny matches, or
// one on a provider it allows only for other models.
func (d *Driver) checkModelProvider(model string) error {
	provider, id, _ := strings.Cut(model, "/")
	v, rule := d.providers.judge(provider, id)
	var msg string
	switch v {
	case permitted:
		return nil
	case modelDenied:
		msg = fmt.Sprintf("create: model %q is denied by %s on this machine (allowed: %s)",
			model, rule, d.providers.describe())
	case modelNotAllowed:
		msg = fmt.Sprintf("create: model %q is on provider %q, where this machine allows only some models and none matches it (allowed: %s)",
			model, provider, d.providers.describe())
	default:
		msg = fmt.Sprintf("create: model %q is on provider %q, which this machine does not allow (allowed: %s)",
			model, provider, d.providers.describe())
	}
	return &fleet.Error{Kind: fleet.ErrorUnsupported, Message: msg, Machine: d.machine}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
