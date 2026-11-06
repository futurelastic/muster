package service

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// muster#176: a respond body whose choices contradict the rest of it is
// the caller's fault, rejected as a 400 in the handler, before the driver —
// which may be a peer's, forwarding the body verbatim — sees it. The empty set matters most: marshalled onward with omitempty it would
// arrive at a peer as {}, which means "accept the highlighted option".
func TestRespondRejectsContradictoryChoicesBeforeAnyDriver(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "operator", Token: "tok", Grants: []Grant{GrantRead, GrantSend}},
	})
	post := func(machine, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/machines/"+machine+"/sessions/s1/respond", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Idempotency-Key", "k-"+machine+body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, machine := range []string{"testbox"} {
		for _, body := range []string{
			`{"choices":[]}`,
			`{"choices":[1],"choice":2}`,
			`{"choices":[1],"cancel":true}`,
			`{"choices":[1,1]}`,
			`{"choices":[0]}`,
		} {
			if got := post(machine, body); got != http.StatusBadRequest {
				t.Errorf("%s %s → %d, want 400", machine, body, got)
			}
		}
		if got := post(machine, `{"choices":[1,3],"nonce":"n"}`); got == http.StatusBadRequest {
			t.Errorf("%s: a well-formed choices body was rejected as invalid", machine)
		}
	}
}

// muster#206: the same boundary for a free-text answer. A body whose text
// is empty, or combined with a choice or a cancel, is the caller's fault and a
// 400 before any driver sees it — an empty free-text field confirmed declines
// the whole dialog, so it must never reach one — and text is held to the byte
// limit input is held to (#114), with the limit named in the refusal.
func TestRespondBoundsFreeTextAtTheHTTPBoundary(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "operator", Token: "tok", Grants: []Grant{GrantRead, GrantSend}},
	})
	post := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/machines/testbox/sessions/s1/respond", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Idempotency-Key", "k-text-"+body[:min(len(body), 24)])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		sb.Write(buf[:n])
		return resp.StatusCode, sb.String()
	}
	for _, body := range []string{
		`{"text":""}`,
		`{"text":"   "}`,
		`{"text":"x","choice":2}`,
		`{"text":"x","cancel":true}`,
	} {
		if got, _ := post(body); got != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", body, got)
		}
	}
	long := `{"text":"` + strings.Repeat("a", defaultMaxInputBytes+1) + `","nonce":"n"}`
	code, msg := post(long)
	if code != http.StatusBadRequest {
		t.Errorf("an over-length text → %d, want 400", code)
	}
	if !strings.Contains(msg, strconv.Itoa(defaultMaxInputBytes)) {
		t.Errorf("the refusal does not name the limit: %s", msg)
	}
	if code, _ := post(`{"text":"a fine answer","nonce":"n"}`); code == http.StatusBadRequest {
		t.Errorf("a well-formed text body was rejected as invalid")
	}
	if code, _ := post(`{"text":"with a tick","choices":[1],"nonce":"n"}`); code == http.StatusBadRequest {
		t.Errorf("text with choices (multi-select) was rejected as invalid")
	}
}
