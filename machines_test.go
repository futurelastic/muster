package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// muster #297 (m): RuntimeInfo gained status, cause and reason. A row from a
// build that predates them must still decode, and read as available.
func TestRuntimeInfo_RowWithoutStatusReadsAsAvailable(t *testing.T) {
	raw := `{"machine":"m","runtime":"r","capabilities":{"deadlineMs":5000,"source":"observed"}}`
	var ri RuntimeInfo
	if err := json.Unmarshal([]byte(raw), &ri); err != nil {
		t.Fatal(err)
	}
	if !ri.Available() {
		t.Errorf("a row with no status read as unavailable: %+v", ri)
	}
	if ri.Status != "" || ri.Cause != "" || ri.Reason != "" {
		t.Errorf("absent fields decoded as %+v", ri)
	}
}

func TestRuntimeInfo_UnavailableRoundTripsAndAnUnknownCauseDecodes(t *testing.T) {
	in := RuntimeInfo{
		Machine: "m", Runtime: "r", Status: RuntimeUnavailable,
		Cause: CauseProbeTimeout, Reason: "did not answer in time",
		Capabilities: DriverCapabilities{DeadlineMs: 30000}.Assumed(),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status":"unavailable"`, `"cause":"probe-timeout"`, `"reason":"did not answer in time"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s missing from %s", want, raw)
		}
	}
	var out RuntimeInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Available() || out.Cause != CauseProbeTimeout || !out.Cause.Retryable() {
		t.Errorf("round trip = %+v", out)
	}

	// A later build may name a cause this one has never heard of.
	var future RuntimeInfo
	err = json.Unmarshal([]byte(`{"machine":"m","runtime":"r","status":"unavailable","cause":"quota-exhausted","capabilities":{"deadlineMs":1,"source":"assumed"}}`), &future)
	if err != nil {
		t.Fatalf("an unknown cause did not decode: %v", err)
	}
	if future.Available() || future.Cause.Retryable() {
		t.Errorf("unknown cause = %+v, want unavailable and not retryable", future)
	}
}

// An available row omits the three fields rather than writing empty ones, so
// a peer's response stays byte-compatible with what an older reader expects.
func TestRuntimeInfo_AvailableRowOmitsCauseAndReason(t *testing.T) {
	raw, _ := json.Marshal(RuntimeInfo{Machine: "m", Runtime: "r", Status: RuntimeAvailable,
		Capabilities: DriverCapabilities{DeadlineMs: 1}.Assumed()})
	if strings.Contains(string(raw), "cause") || strings.Contains(string(raw), "reason") {
		t.Errorf("available row carries cause/reason: %s", raw)
	}
}
