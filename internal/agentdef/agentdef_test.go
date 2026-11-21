package agentdef

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

func write(t *testing.T, name, body string) fleet.AbsolutePath {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fleet.AbsolutePath(p)
}

func TestReadIndex_ReadsTheFileAsItIsNow(t *testing.T) {
	p := write(t, "MEMORY.md", "one\n")
	got, err := ReadIndex(p)
	if err != nil || string(got) != "one\n" {
		t.Fatalf("ReadIndex = %q, %v", got, err)
	}
	if err := os.WriteFile(string(p), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadIndex(p); string(got) != "two\n" {
		t.Errorf("a second read = %q, want the file as it is now: nothing is cached", got)
	}
}

func TestReadIndex_AnEmptyIndexIsAllowed(t *testing.T) {
	if got, err := ReadIndex(write(t, "MEMORY.md", "")); err != nil || len(got) != 0 {
		t.Fatalf("an empty index = %q, %v; an agent whose memory was just made has nothing in it yet", got, err)
	}
}

func TestReadIndex_Refusals(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		path fleet.AbsolutePath
		want string
	}{
		{"missing", fleet.AbsolutePath(filepath.Join(dir, "absent.md")), "cannot be read"},
		{"a directory", fleet.AbsolutePath(dir), "a directory"},
		{"too large", write(t, "big.md", strings.Repeat("x", fleet.AgentKnowledgeMaxIndexBytes+1)), "larger than"},
		{"not text", write(t, "bin.md", "\xff\xfe"), "UTF-8"},
	}
	for _, c := range cases {
		_, err := ReadIndex(c.path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", c.name, err, c.want)
		}
	}
}

func TestReadIndex_AnErrorNeverQuotesTheContent(t *testing.T) {
	_, err := ReadIndex(write(t, "bin.md", "SECRET-CONTENT\xff"))
	if err == nil || strings.Contains(err.Error(), "SECRET-CONTENT") {
		t.Fatalf("err = %v, want a refusal that does not quote the file", err)
	}
}

func TestNote_NamesTheHomeAndWhatTheSessionMayDoThere(t *testing.T) {
	k := fleet.AgentKnowledge{Index: "/m/MEMORY.md", Home: "/m"}
	ro := Note(k, []byte("- a line"))
	if !strings.Contains(ro, "`/m`") || !strings.Contains(ro, "may read files there") || strings.Contains(ro, "write") || !strings.Contains(ro, "- a line\n") {
		t.Errorf("read-only note:\n%s", ro)
	}
	k.Write = true
	if rw := Note(k, nil); !strings.Contains(rw, "read and write") {
		t.Errorf("writable note:\n%s", rw)
	}
}
