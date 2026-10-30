package opencode

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	fleet "github.com/futurelastic/muster"
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
func (d *Driver) readCatalog(ctx context.Context, srv *server) (cat modelCatalog, read catalogRead, err error) {
	var wire wireProviders
	if err := d.do(ctx, srv, "GET", "/config/providers", nil, &wire); err != nil {
		if isUnreachable(err) {
			return nil, catalogUnknown, err
		}
		return nil, catalogUnknown, nil
	}
	if wire.Providers == nil {
		return nil, catalogUnknown, nil
	}
	for _, p := range wire.Providers {
		for id := range p.Models {
			cat = append(cat, p.ID+"/"+id)
		}
	}
	sort.Strings(cat)
	if len(cat) == 0 {
		return nil, catalogEmpty, nil
	}
	return cat, catalogListed, nil
}

// within returns the entries whose provider the policy permits.
func (c modelCatalog) within(p providerPolicy) modelCatalog {
	var out modelCatalog
	for _, id := range c {
		if p.permits(providerOfModel(id)) {
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
	cat, read, err := d.readCatalog(ctx, srv)
	if err != nil {
		return err
	}
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
		cat, read, err := d.readCatalog(ctx, srv)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if read != catalogListed {
			continue
		}
		seen = seen.union(cat.within(d.providers))
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
		seen := map[string]bool{}
		var skipped []string
		for _, id := range listed {
			if p := providerOfModel(id); !seen[p] {
				seen[p] = true
				skipped = append(skipped, p)
			}
		}
		msg += "; it lists only " + strings.Join(quoteAll(skipped), ", ")
	} else {
		msg += "; it lists no provider at all, so the session has no credential for any"
	}
	msg += ". Give the session a credential for an allowed provider, or change the operator's provider setting; " +
		"a session is never started on a provider nobody chose"
	return &fleet.Error{Kind: fleet.ErrorUnsupported, Message: msg, Machine: d.machine}
}

// checkModelProvider refuses, before anything is started, a model whose
// provider the policy does not allow.
func (d *Driver) checkModelProvider(model string) error {
	if p := providerOfModel(model); !d.providers.permits(p) {
		return &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: fmt.Sprintf("create: model %q is on provider %q, which this machine does not allow (allowed: %s)",
				model, p, d.providers.describe()),
			Machine: d.machine,
		}
	}
	return nil
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
