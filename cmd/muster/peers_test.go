package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func peersFrom(t *testing.T, doc string) *fileConfig {
	t.Helper()
	var cfg fileConfig
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func flat(es []peerEntry) map[string][]string {
	m := map[string][]string{}
	for _, e := range es {
		m[string(e.machine)] = e.urls
	}
	return m
}

// #335: a config that lists one `url` per peer is read exactly as it always was.
func TestResolvePeersSingleURLIsUnchanged(t *testing.T) {
	cfg := peersFrom(t, `{"peers":[{"machine":"a","url":"http://a:1","token":"x"},{"machine":"b","url":"http://b:1"}]}`)
	got, err := resolvePeers("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"a": {"http://a:1"}, "b": {"http://b:1"}}
	if !reflect.DeepEqual(flat(got), want) {
		t.Errorf("got %v, want %v", flat(got), want)
	}
	if len(got) != 2 || got[0].machine != "a" {
		t.Errorf("order must be the file's: %v", got)
	}
}

func TestResolvePeersURLsKeepTheirOrder(t *testing.T) {
	cfg := peersFrom(t, `{"peers":[{"machine":"laptop","urls":["http://10.1.1.5:1"," http://10.2.2.5:1 ",""]}]}`)
	got, err := resolvePeers("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http://10.1.1.5:1", "http://10.2.2.5:1"}; !reflect.DeepEqual(got[0].urls, want) {
		t.Errorf("got %v, want %v", got[0].urls, want)
	}
}

func TestResolvePeersRefusesBothURLAndURLs(t *testing.T) {
	cfg := peersFrom(t, `{"peers":[{"machine":"laptop","url":"http://a:1","urls":["http://b:1"]}]}`)
	_, err := resolvePeers("", cfg)
	if err == nil || !strings.Contains(err.Error(), "both url and urls") {
		t.Errorf("want a refusal naming the conflict, got %v", err)
	}
}

func TestResolvePeersRefusesAnEntryWithNoAddress(t *testing.T) {
	for _, doc := range []string{
		`{"peers":[{"machine":"laptop"}]}`,
		`{"peers":[{"machine":"laptop","urls":[]}]}`,
		`{"peers":[{"url":"http://a:1"}]}`,
	} {
		if _, err := resolvePeers("", peersFrom(t, doc)); err == nil {
			t.Errorf("%s: want a refusal", doc)
		}
	}
}

// FLEET_PEERS wins over the file, and a machine named twice is one peer with
// several addresses, in the order given.
func TestResolvePeersEnv(t *testing.T) {
	cfg := peersFrom(t, `{"peers":[{"machine":"ignored","url":"http://z:1"}]}`)
	got, err := resolvePeers("laptop=http://a:1, other=http://o:1 ,laptop=http://b:1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"laptop": {"http://a:1", "http://b:1"}, "other": {"http://o:1"}}
	if !reflect.DeepEqual(flat(got), want) {
		t.Errorf("got %v, want %v", flat(got), want)
	}
	if got[0].machine != "laptop" {
		t.Errorf("order of first appearance must hold: %v", got)
	}
	for _, bad := range []string{"noequals", "=http://a:1", "a="} {
		if _, err := resolvePeers(bad, nil); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}

func TestResolvePeersNoneConfigured(t *testing.T) {
	if got, err := resolvePeers("", nil); err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// The doctor reads the list through the same function, so a peer with several
// addresses is probed on each in order and the row names the one that answered.
func TestDoctorPeersCarryEveryAddress(t *testing.T) {
	cfg := peersFrom(t, `{"peers":[{"machine":"laptop","urls":["http://a:1","http://b:1"],"token":"tok"}]}`)
	got, err := doctorPeers(func(string) string { return "" }, cfg)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if !reflect.DeepEqual(got[0].bases, []string{"http://a:1", "http://b:1"}) || got[0].token != "tok" {
		t.Errorf("got %+v", got[0])
	}
}
