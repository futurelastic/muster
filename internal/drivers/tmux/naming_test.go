package tmux

import (
	"context"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "drops control characters", in: "\x01alpha\x02", want: "alpha"},
		{name: "drops colon", in: "a:b", want: "ab"},
		{name: "turns dot to hyphen", in: "a.b", want: "a-b"},
		{name: "trims leading hyphens", in: "--alpha", want: "alpha"},
		{name: "keeps clean name", in: "already-clean", want: "already-clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeName(tc.in); got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecoration(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "all body chars", in: "alpha-1", want: ""},
		{name: "trailing non-body", in: "alpha💬", want: "💬"},
		{name: "entire non-body", in: "§", want: ""},
		{name: "mixed trailing non-body only", in: "alpha-§", want: "§"},

		// #360: a letter or number in any script is body, not decoration.
		{name: "accented Latin letter is body", in: "báo-cáo-quý", want: ""},
		{name: "accented Latin letter before a marker", in: "báo-cáo-quý💬", want: "💬"},
		{name: "Cyrillic letter is body", in: "отчёт", want: ""},
		{name: "Cyrillic letter before a marker", in: "отчёт💬", want: "💬"},
		{name: "CJK ideographs are body", in: "月次報告", want: ""},
		{name: "CJK ideographs before a marker", in: "月次報告💬", want: "💬"},
		{name: "kana is body", in: "ほうこく", want: ""},
		{name: "non-ASCII digit is body", in: "report-٣", want: ""},
		{name: "uppercase ASCII is body", in: "alphaB", want: ""},
		{name: "uppercase ASCII before a marker", in: "alphaB💬", want: "💬"},
		{name: "separators are body", in: "alpha_b.c-", want: ""},

		// A combining mark takes the class of the rune it follows.
		{name: "NFD accent stays with its letter", in: "quy\u0301", want: ""},
		{name: "NFD accent before a marker", in: "quy\u0301💬", want: "💬"},
		{name: "variation selector stays with its symbol", in: "alpha⚙\ufe0f", want: "⚙\ufe0f"},
		{name: "variation selector with a letter-ending name", in: "báo⚙\ufe0f", want: "⚙\ufe0f"},
		{name: "ZWJ sequence is carried whole", in: "alpha👨\u200d💻", want: "👨\u200d💻"},
		{name: "mark with no base is decoration", in: "\u0301", want: ""},
		{name: "leading orphan mark is not a body rune", in: "\u0301a", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decoration(tc.in); got != tc.want {
				t.Errorf("decoration(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyMarker(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		marker string
		known  markerState
		want   string
	}{
		{name: "marker already present (both body)", inName: "alpha-1", marker: "alpha-1", known: markerUnknown, want: "alpha-1"},
		{name: "marker already present after stacking", inName: "alpha-1alpha-1", marker: "alpha-1", known: markerUnknown, want: "alpha-1alpha-1"},
		{name: "non-body marker not yet present", inName: "alpha", marker: "§", known: markerUnknown, want: "alpha§"},
		{name: "non-body marker already present", inName: "alpha§", marker: "§", known: markerUnknown, want: "alpha§"},
		{name: "empty marker leaves name", inName: "alpha", marker: "", known: markerUnknown, want: "alpha"},
		{name: "existing decoration prevents appending a different marker", inName: "alpha§", marker: "alpha-1", known: markerUnknown, want: "alpha§"},

		// #360: a name ending in a non-ASCII letter is not already decorated,
		// so the caller's marker is carried rather than silently dropped.
		{name: "accented Latin name gets its marker", inName: "báo-cáo-quý", marker: "💬", known: markerUnknown, want: "báo-cáo-quý💬"},
		{name: "Cyrillic name gets its marker", inName: "отчёт", marker: "💬", known: markerUnknown, want: "отчёт💬"},
		{name: "CJK name gets its marker", inName: "月次報告", marker: "💬", known: markerUnknown, want: "月次報告💬"},
		{name: "uppercase ASCII name gets its marker", inName: "alphaB", marker: "💬", known: markerUnknown, want: "alphaB💬"},
		{name: "NFD name gets its marker", inName: "quy\u0301", marker: "💬", known: markerUnknown, want: "quy\u0301💬"},
		{name: "marker with a variation selector is carried whole", inName: "báo-cáo-quý", marker: "⚙\ufe0f", known: markerUnknown, want: "báo-cáo-quý⚙\ufe0f"},
		{name: "accented Latin name already marked is not stacked", inName: "báo-cáo-quý💬", marker: "💬", known: markerUnknown, want: "báo-cáo-quý💬"},
		{name: "CJK name already marked is not stacked", inName: "月次報告💬", marker: "💬", known: markerUnknown, want: "月次報告💬"},
		{name: "variation-selector marker already present is not stacked", inName: "báo-cáo-quý⚙\ufe0f", marker: "⚙\ufe0f", known: markerUnknown, want: "báo-cáo-quý⚙\ufe0f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyMarker(tc.inName, tc.marker, tc.known); got != tc.want {
				t.Errorf("applyMarker(%q, %q, %v) = %q, want %q", tc.inName, tc.marker, tc.known, got, tc.want)
			}
		})
	}
}

// muster #96: once a session record answers the question, applyMarker
// stops guessing from the string — including in the exact case the old
// heuristic could not tell apart, a marker drawn from the same alphabet as
// the name body that coincidentally matches the name's own trailing
// characters.
func TestApplyMarkerIsExactWhenTheRecordKnows(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		marker string
		known  markerState
		want   string
	}{
		{
			name:   "record says absent: append even though the string coincidentally ends in the marker",
			inName: "append", marker: "end", known: markerAbsent,
			want: "appendend",
		},
		{
			name:   "record says applied: leave it, no string comparison needed",
			inName: "alpha-1", marker: "alpha-1", known: markerApplied,
			want: "alpha-1",
		},
		{
			name:   "record says applied, non-body marker: still exact",
			inName: "alpha§", marker: "§", known: markerApplied,
			want: "alpha§",
		},
		{
			name:   "record says absent, non-body marker: still appends",
			inName: "alpha", marker: "§", known: markerAbsent,
			want: "alpha§",
		},
		{
			name:   "empty marker leaves name regardless of known state",
			inName: "alpha", marker: "", known: markerApplied,
			want: "alpha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyMarker(tc.inName, tc.marker, tc.known); got != tc.want {
				t.Errorf("applyMarker(%q, %q, %v) = %q, want %q", tc.inName, tc.marker, tc.known, got, tc.want)
			}
		})
	}
}

func TestNumberedName(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		n      int
		want   string
	}{
		{name: "n < 2 returns name", inName: "alpha💬", n: 1, want: "alpha💬"},
		{name: "counter before non-ASCII marker", inName: "alpha💬", n: 2, want: "alpha-2💬"},
		{name: "ascii-alphabet suffix is not mistaken for a marker (issue #90)", inName: "append", n: 2, want: "append-2"},
		{name: "no marker, decoration empty", inName: "alpha", n: 2, want: "alpha-2"},

		// #360: the counter goes after the whole last word, never inside it.
		{name: "accented Latin name, no marker", inName: "báo-cáo-quý", n: 2, want: "báo-cáo-quý-2"},
		{name: "accented Latin name, counter before marker", inName: "báo-cáo-quý💬", n: 2, want: "báo-cáo-quý-2💬"},
		{name: "Cyrillic name, counter before marker", inName: "отчёт💬", n: 3, want: "отчёт-3💬"},
		{name: "CJK name, no marker", inName: "月次報告", n: 2, want: "月次報告-2"},
		{name: "CJK name, counter before marker", inName: "月次報告💬", n: 2, want: "月次報告-2💬"},
		{name: "uppercase ASCII name, counter before marker", inName: "alphaB💬", n: 2, want: "alphaB-2💬"},
		{name: "NFD name, counter after the combined letter", inName: "quy\u0301", n: 2, want: "quy\u0301-2"},
		{name: "marker with a variation selector moves as one unit", inName: "báo-cáo-quý⚙\ufe0f", n: 2, want: "báo-cáo-quý-2⚙\ufe0f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := numberedName(tc.inName, tc.n); got != tc.want {
				t.Errorf("numberedName(%q, %d) = %q, want %q", tc.inName, tc.n, got, tc.want)
			}
		})
	}
}

func TestResolveNameIdempotent(t *testing.T) {
	ctx := context.Background()

	t.Run("ASCII marker", func(t *testing.T) {
		d := newTestDriver(&fakeMux{})
		first, _, ok := d.resolveName(ctx, "alpha-1", "alpha-1")
		if !ok || first != "alpha-1" {
			t.Fatalf("first = %q, ok=%v, want %q", first, ok, "alpha-1")
		}
		second, _, ok := d.resolveName(ctx, first, "alpha-1")
		if !ok || second != first {
			t.Fatalf("second = %q, ok=%v, want same %q", second, ok, first)
		}
	})

	t.Run("non-nameBody marker", func(t *testing.T) {
		d := newTestDriver(&fakeMux{})
		first, _, ok := d.resolveName(ctx, "alpha", "§")
		if !ok || first != "alpha§" {
			t.Fatalf("first = %q, ok=%v, want %q", first, ok, "alpha§")
		}
		second, _, ok := d.resolveName(ctx, first, "§")
		if !ok || second != first {
			t.Fatalf("second = %q, ok=%v, want same %q", second, ok, first)
		}
	})
}

// #360, end to end: a name typed in another script resolves to the same shape
// an ASCII one does — marker carried, counter ahead of it, and the second
// resolution of an already-resolved name changes nothing.
func TestResolveNameNonASCII(t *testing.T) {
	ctx := context.Background()
	const marker = "💬"

	for _, requested := range []string{"báo-cáo-quý", "báo-cáo-tài-chính", "отчёт", "月次報告", "Report"} {
		t.Run(requested, func(t *testing.T) {
			d := newTestDriver(&fakeMux{})
			got, applied, ok := d.resolveName(ctx, requested, marker)
			if !ok || got != requested+marker || !applied {
				t.Fatalf("resolveName(%q) = %q, applied=%v, ok=%v, want %q applied", requested, got, applied, ok, requested+marker)
			}
			again, _, ok := d.resolveName(ctx, got, marker)
			if !ok || again != got {
				t.Fatalf("second resolve = %q, ok=%v, want the same %q", again, ok, got)
			}
		})
	}
}

// #360, the collision half: a second session asking for the same non-ASCII
// name is numbered after the last word and ahead of the marker, exactly as an
// ASCII name is. Before, "báo-cáo-quý" came back as "báo-cáo-qu-2ý".
func TestResolveNameNonASCIICollision(t *testing.T) {
	ctx := context.Background()
	const marker = "💬"

	for _, requested := range []string{"báo-cáo-quý", "отчёт", "月次報告"} {
		t.Run(requested, func(t *testing.T) {
			f := &fakeMux{captures: map[string]string{}}
			f.addSession(fakeSession{name: requested + marker, paneID: "%1"}, "")
			d := newTestDriver(f)
			got, _, ok := d.resolveName(ctx, requested, marker)
			if want := requested + "-2" + marker; !ok || got != want {
				t.Fatalf("resolveName(%q) = %q, ok=%v, want %q", requested, got, ok, want)
			}
		})
	}
}
