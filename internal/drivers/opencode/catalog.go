package opencode

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

// readCatalog asks the session's own server which models it would accept.
//
// ok is false — and err nil — when the runtime gave no usable answer: the
// endpoint is absent (an older runtime), it answered an error, or it listed no
// model at all. That is "this driver could not tell", not "the model is
// wrong", so the create proceeds exactly as it did before the check existed
// (§5.7: absence of evidence is not evidence of absence). A server that does
// not answer at all is an error, because the create would fail on it anyway.
func (d *Driver) readCatalog(ctx context.Context, srv *server) (cat modelCatalog, ok bool, err error) {
	var wire wireProviders
	if err := d.do(ctx, srv, "GET", "/config/providers", nil, &wire); err != nil {
		if isUnreachable(err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	for _, p := range wire.Providers {
		for id := range p.Models {
			cat = append(cat, p.ID+"/"+id)
		}
	}
	sort.Strings(cat)
	return cat, len(cat) > 0, nil
}

// checkModel refuses a create whose model the runtime's catalog does not list.
// The catalog is read from the runtime on every create, never compiled in:
// catalogs change with runtime releases and with the credentials a session was
// given.
func (d *Driver) checkModel(ctx context.Context, srv *server, model string) error {
	cat, ok, err := d.readCatalog(ctx, srv)
	if err != nil {
		return err
	}
	if !ok || cat.has(model) {
		return nil
	}
	msg := fmt.Sprintf("create: model %q is not in the runtime's own catalog", model)
	if near := cat.closest(model, 3); len(near) > 0 {
		msg += "; closest: " + strings.Join(quoteAll(near), ", ")
	}
	return &fleet.Error{Kind: fleet.ErrorInvalid, Message: msg, Machine: d.machine}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
