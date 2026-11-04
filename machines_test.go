package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// muster #400: RuntimeInfo gained models. A row from a build that predates it
// decodes with none, and a list never reaches the wire with a null in it.
func TestRuntimeInfo_ModelsRoundTripAndOlderRowsDecode(t *testing.T) {
	old := `{"machine":"m","runtime":"r","capabilities":{"deadlineMs":5000,"source":"observed"}}`
	var ri RuntimeInfo
	if err := json.Unmarshal([]byte(old), &ri); err != nil {
		t.Fatal(err)
	}
	if ri.Models != nil {
		t.Errorf("a row without models decoded as %+v, want nil (the service reads that as none)", ri.Models)
	}

	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	def := "deepseek/deepseek-flash"
	in := RuntimeInfo{Machine: "m", Runtime: "r",
		Capabilities: DriverCapabilities{DeadlineMs: 5000, Source: CapabilitiesObserved}, Models: &RuntimeModels{
			Source: ModelsFromRuntime, ReadAt: &at, IDs: []string{def}, Default: &def,
		}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back RuntimeInfo
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Models == nil || back.Models.Source != ModelsFromRuntime || !back.Models.ReadAt.Equal(at) ||
		len(back.Models.IDs) != 1 || *back.Models.Default != def {
		t.Errorf("round trip lost the block: %s -> %+v", b, back.Models)
	}
}

func TestRuntimeModels_NoneHasNoNullAndNormalizedFixesLooseValues(t *testing.T) {
	b, err := json.Marshal(NoModels())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"source":"none","ids":[],"default":null}` {
		t.Errorf("NoModels() = %s", got)
	}
	// A source nobody stated is "none": a list with no origin is not offered.
	if got := (RuntimeModels{IDs: []string{"x"}}).Normalized(); got.Source != ModelsNone || len(got.IDs) != 0 {
		t.Errorf("a list with no source: %+v, want none and no ids", got)
	}
	// A declared list with nil ids keeps its source and gets an empty list.
	if got := (RuntimeModels{Source: ModelsDeclared}).Normalized(); got.Source != ModelsDeclared || got.IDs == nil {
		t.Errorf("a declared list with nil ids: %+v, want declared and []", got)
	}
}
