package service

import (
	"encoding/json"
	"errors"
	"net/http"

	fleet "github.com/futurelastic/muster"
)

// PostureDryRunRow is one runtime's answer to a posture dry run (muster #344):
// whether the runtime can enforce it and, if so, what it would be lowered onto.
type PostureDryRunRow struct {
	Machine fleet.MachineId `json:"machine"`
	Runtime fleet.RuntimeId `json:"runtime"`
	// Enforceable is false when the runtime would refuse the create, and Reason
	// says why. A runtime whose capabilities nobody has obtained (a peer not yet
	// reached) is enforceable "unknown": Known is false and Enforceable true
	// means only "not refused here" — the peer decides.
	Enforceable bool   `json:"enforceable"`
	Known       bool   `json:"known"`
	Reason      string `json:"reason,omitempty"`
	// Mapping is the per-runtime fields the posture lowers to, present when
	// Enforceable.
	Mapping *fleet.PostureMapping `json:"mapping,omitempty"`
}

// PostureDryRun lowers p against every runtime GET /v1/runtimes lists, changing
// nothing. It runs the same fleet.Posture.Lower a create runs, so the answer
// cannot differ from what a create would do.
func (s *Service) PostureDryRun(r *http.Request, p fleet.Posture) ([]PostureDryRunRow, error) {
	col, err := s.ListRuntimes(r.Context())
	if err != nil {
		return nil, err
	}
	items := col.Items()
	rows := make([]PostureDryRunRow, 0, len(items))
	for _, ri := range items {
		row := PostureDryRunRow{Machine: ri.Machine, Runtime: ri.Runtime}
		if !ri.Available() {
			row.Known = true
			row.Reason = "runtime unavailable: " + ri.Reason
			rows = append(rows, row)
			continue
		}
		row.Known = ri.Capabilities.Source != fleet.CapabilitiesAssumed
		m, lerr := p.Lower(ri.Capabilities)
		if lerr != nil {
			var fe *fleet.Error
			if errors.As(lerr, &fe) {
				row.Reason = fe.Message
			} else {
				row.Reason = lerr.Error()
			}
			rows = append(rows, row)
			continue
		}
		row.Enforceable = true
		row.Mapping = &m
		rows = append(rows, row)
	}
	return rows, nil
}

// handlePostureDryRun answers POST /v1/posture/dry-run: `{"posture": {...}}` in,
// one row per runtime out. Nothing is started, claimed or recorded.
func handlePostureDryRun(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Posture *fleet.Posture `json:"posture"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body: send {\"posture\": {...}} (#344)"})
			return
		}
		if body.Posture == nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "posture is required (#344)"})
			return
		}
		if err := body.Posture.Validate(); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error() + " (#344)"})
			return
		}
		rows, err := svc.PostureDryRun(r, *body.Posture)
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runtimes": rows})
	}
}
