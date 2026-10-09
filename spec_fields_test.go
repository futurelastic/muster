package fleet

// This file is the mechanical check muster issue #57 asked to be left
// behind, rather than performed once: a comparison of every JSON-tagged
// field on the wire types below against the normative pseudocode block that
// names them in docs/spec/session-abstraction.md or docs/spec/api-http.md.
//
// # Why a parser instead of a hand-written manifest
//
// A hand-maintained list of "fields the docs mention" would drift from the
// docs exactly the way the docs drifted from the code — it is one more place
// to forget to update, and forgetting to update the normative doc is the
// entire failure this file exists to catch. Reading the ``` fenced
// pseudocode blocks directly means the only source of truth is the prose a
// person reads, which is the property #57 wants.
//
// # Why this cannot cry wolf
//
// Three deliberate narrowings keep this from failing on change that carries
// no information:
//
//   - It compares FIELD NAMES only, never types, tags-minus-name, or
//     ordering. A field that changes shape without being renamed or
//     added/removed does not fire this check — that is a real gap (see
//     docs/spec/checks.md), and is not this file's job.
//   - specFieldExceptions is a real escape hatch, not a bypass: an entry
//     here asserts a field is deliberately absent from the model doc — e.g.
//     a driver-internal concern living on a shared type — and must carry a
//     reason a reviewer can check against the type's own doc comment. A
//     field that is simply forgotten must not be silenced by adding it
//     here; it must be documented or genuinely justified.
//   - Only types with a clean 1:1 Go struct <-> named pseudocode block are
//     registered (specFieldTypes). A type with no formal block in the spec
//     (Session itself — see docs/spec/session-abstraction.md's "Optional on
//     Session" prose, which never gives Session its own block) is not
//     checked here because there is nothing mechanical to compare against;
//     that gap is recorded in docs/spec/checks.md instead of being faked by
//     this test.
//
// Run it directly with: go test -run TestSpecTypeBlocksMatchGoFields -v

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// specDocPaths are the two normative documents. session-abstraction.md is
// authoritative for the model; api-http.md is the wire protocol built on it
// and sometimes documents a field (in prose or a JSON example) that the
// model document's own pseudocode block omits — api-http.md §0's own words:
// "Where the two disagree, the abstraction wins and this document is the
// bug." Both are scanned so that a field documented in EITHER counts as
// documented; this test's job is to catch a field missing from BOTH, which
// is what leaves a reader with no normative text to find it in at all.
var specDocPaths = []string{
	"docs/spec/session-abstraction.md",
	"docs/spec/api-http.md",
}

// specFieldTypes registers every Go wire type this check holds to the
// standard of "every field appears in a named pseudocode block somewhere in
// docs/spec". Add a type here when you give it one; the zero value is
// enough, reflection only looks at field names and json tags.
var specFieldTypes = map[string]any{
	"SessionSpec":        SessionSpec{},
	"SessionRef":         SessionRef{},
	"SessionState":       SessionState{},
	"DeliveryReceipt":    DeliveryReceipt{},
	"Ack":                Ack{},
	"Request":            Request{},
	"Caller":             Caller{},
	"Expectation":        Expectation{},
	"SessionPrompt":      SessionPrompt{},
	"Response":           Response{},
	"AttachHint":         AttachHint{},
	"ConversationRef":    ConversationRef{},
	"DriverCapabilities": DriverCapabilities{},
	"SourceStatus":       SourceStatus{},
	"PromptDelivery":     PromptDelivery{},
	"PinOutcome":         PinOutcome{},
	"PinResult":          PinResult{},
	"RuntimeSurfaceRef":  RuntimeSurfaceRef{},
	"IdentityAssertion":  IdentityAssertion{},
	"PeerStanding":       PeerStanding{},

	// #185: the optional external delivery module surface.
	"DeliveryPath":         DeliveryPath{},
	"DeliveryLane":         DeliveryLane{},
	"DeliveryModuleStatus": DeliveryModuleStatus{},

	// #222: rename's own response, and its title-sync half.
	"RenameAck": RenameAck{},
	"TitleSync": TitleSync{},

	// #230: footer notices read below the composer.
	"Warning": Warning{},

	// #285: the runtime's own account of what a session spent.
	"Usage":     Usage{},
	"TurnUsage": TurnUsage{},
}

// specFieldExceptions records a Go field that a normative type block
// deliberately does not carry, and why. Every entry MUST cite the decision
// that justifies it (an issue number, or the resolving prose in the spec)
// so a reviewer can tell "decided" from "forgotten" without re-deriving the
// argument.
//
// Empty as of muster issue #59: the last outstanding entry
// (SessionState.screenDigest) was removed when #59's ruling promoted the
// field into session-abstraction.md §2.3 rather than leaving it excepted.
// Left as a map, not deleted, because a future genuinely-driver-internal
// field needs somewhere to register the same argument.
var specFieldExceptions = map[string]map[string]string{}

// TestSpecTypeBlocksMatchGoFields is muster issue #57's mechanical
// check: it fails when a registered Go type carries a JSON field absent
// from every normative pseudocode block that names it, and (the direction
// #57 did not hit but is the same class of bug) when a pseudocode block
// names a field the Go type no longer has.
func TestSpecTypeBlocksMatchGoFields(t *testing.T) {
	docFields, err := parseSpecTypeBlocks(specDocPaths)
	if err != nil {
		t.Fatalf("parsing spec docs: %v", err)
	}

	names := make([]string, 0, len(specFieldTypes))
	for name := range specFieldTypes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sample := specFieldTypes[name]
		goFields := jsonFieldNames(sample)

		doc, ok := docFields[name]
		if !ok {
			t.Errorf("%s: no normative type block named %q found in %s — "+
				"either it was renamed (update specFieldTypes) or the block "+
				"was deleted from the spec (restore it)", name, name, strings.Join(specDocPaths, " or "))
			continue
		}
		docSet := toSet(doc)
		goSet := toSet(goFields)
		exceptions := specFieldExceptions[name]

		var undocumented []string
		for _, f := range goFields {
			if docSet[f] {
				continue
			}
			if exceptions != nil {
				if _, excepted := exceptions[f]; excepted {
					continue
				}
			}
			undocumented = append(undocumented, f)
		}
		if len(undocumented) > 0 {
			sort.Strings(undocumented)
			t.Errorf("%s: field(s) %s exist on the Go type but appear in no "+
				"normative type block in %s. Either add the field to the "+
				"%s{} block (session-abstraction.md and/or api-http.md), "+
				"or — if it is genuinely driver detail that should not be "+
				"normative — add it to specFieldExceptions with the reason, "+
				"per this file's own rule.",
				name, strings.Join(undocumented, ", "), strings.Join(specDocPaths, " or "), name)
		}

		var stale []string
		for _, f := range doc {
			if goSet[f] {
				continue
			}
			stale = append(stale, f)
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			t.Errorf("%s: normative block names field(s) %s that no longer "+
				"exist on the Go type — the spec is describing a shape the "+
				"code does not have. Fix the %s{} block.",
				name, strings.Join(dedupe(stale), ", "), name)
		}
	}
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func dedupe(ss []string) []string {
	seen := toSet(ss)
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// jsonFieldNames returns the wire name of every exported field of v, in the
// same sense encoding/json would use it: the json tag's name segment when
// present and not "-", otherwise the field name with its initial rune
// lower-cased to match this codebase's tagless internal types (Request,
// Caller, Expectation), whose fields are never marshaled directly but whose
// spec block uses lowerCamel names for them all the same.
func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag, hasTag := f.Tag.Lookup("json")
		if hasTag {
			if tag == "-" {
				continue
			}
			name := strings.SplitN(tag, ",", 2)[0]
			if name == "" {
				name = lowerFirst(f.Name)
			}
			names = append(names, name)
			continue
		}
		names = append(names, lowerFirst(f.Name))
	}
	return names
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// specTypeOpenRe matches the opening line of a named pseudocode block, e.g.
// "SessionState {" or "Collection<T> {". Only lines that are ENTIRELY the
// opening (plus an optional trailing comment) match, so operation-table
// lines like "create(req, spec) -> SessionRef" and wire examples like
// `→ 200 { "epoch": ...` never do.
var specTypeOpenRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)(?:<[^>]*>)?\s*\{\s*(//.*)?$`)

// specFieldLineRe matches a field declaration line inside a block, e.g.
// "  agent?     : AgentId" or "  supportsPin : { model: boolean, ... }" —
// the latter is captured as a single field named "supportsPin"; this parser
// deliberately does not recurse into an inline nested object, so a drift
// inside PinSupport's three booleans is not this check's concern (see the
// file doc comment's "cannot cry wolf" list).
var specFieldLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\??\s*:`)

// parseSpecTypeBlocks scans every ``` fenced block in each path for
// top-level "Identifier {" ... "}" pseudocode, and returns every field name
// declared directly inside each named block. A name occurring in more than
// one block (unexpected for this spec today) has its fields unioned.
func parseSpecTypeBlocks(paths []string) (map[string][]string, error) {
	result := map[string][]string{}
	for _, path := range paths {
		lines, err := readLines(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		inFence := false
		for i := 0; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "```") {
				inFence = !inFence
				continue
			}
			if !inFence {
				continue
			}
			m := specTypeOpenRe.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			name := m[1]
			fields, next := parseOneBlock(lines, i+1)
			result[name] = append(result[name], fields...)
			i = next - 1 // the outer loop's i++ resumes right after the block
		}
	}
	return result, nil
}

// parseOneBlock reads the body of one already-opened block starting at
// lines[start], tracking brace depth so a same-line nested object (like
// supportsPin's) is captured as one field and does not end the block early.
// It returns the field names found and the index of the first line after
// the block (or after the file/fence ends, if the block was never closed —
// treated as "no more fields", not a parse error, since a malformed fence
// is a docs bug this test's OTHER failures will already be loud about).
func parseOneBlock(lines []string, start int) ([]string, int) {
	depth := 1
	var fields []string
	i := start
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") {
			break
		}
		if depth == 1 {
			if fm := specFieldLineRe.FindStringSubmatch(t); fm != nil {
				fields = append(fields, fm[1])
			}
		}
		depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		if depth <= 0 {
			i++
			break
		}
	}
	return fields, i
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}
