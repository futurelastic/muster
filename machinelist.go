package fleet

// MachineListReceipt is the answer to a pushed machine list that was taken
// (PUT /v1/machine-list, api-http.md §3.1, muster #352).
//
// It is returned only when the list was written AND is in force: a list that
// was refused, for any reason, is an error and nothing on the machine changed.
type MachineListReceipt struct {
	// Fleet is the fleet's id, from the list.
	Fleet string `json:"fleet"`
	// Seq is the sequence now in force on this machine.
	Seq uint64 `json:"seq"`
	// PreviousSeq is the highest sequence this machine had accepted before this
	// one; 0 when it had accepted none.
	PreviousSeq uint64 `json:"previousSeq"`
	// Mode is the machineList mode this machine runs: "shadow" (in force for
	// reporting only, peers unchanged), "prefer" or "only".
	Mode string `json:"mode"`
	// Machines is how many machines the list names.
	Machines int `json:"machines"`
}
