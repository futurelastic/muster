package fleet

// ScratchUsage is how much a session's scratch directory holds (muster #419): the
// temporary files its runtime and its tools wrote while it ran.
//
// # Why it is on the state
//
// A session that waits for a person for a day can sit on gigabytes of test
// fixtures and build output in its temporary directory, and nothing said so until
// the disk was full. The directory is removed when the session is closed; until
// then it is only the size of it that is worth knowing, and it is a fact about the
// session, so it travels with the state a poller already reads.
//
// # What the figure is
//
// The sum of the sizes of the regular files under the directory, as the file
// system reports them: not the space they occupy, and a link is not followed.
// Bytes is measured, not estimated, and it is not live — a driver measures at most
// every few minutes, never on the request path — so AsOf says when, and a reader
// seeing a size that stopped moving can tell. Partial is true when the walk was cut
// short by its bound (a very large tree), in which case Bytes is a lower bound.
//
// Absent means no figure: the driver keeps no such directory for the session, or
// has not measured it yet. Never a zero size (§5.7). It is not a material change
// (MateriallyDiffers): it moves with every file written, so a feed that fired on it
// would storm.
type ScratchUsage struct {
	// Bytes is the total size of the regular files under the scratch directory.
	Bytes int64 `json:"bytes"`
	// AsOf is when the directory was measured, not when this state was read.
	AsOf Timestamp `json:"asOf"`
	// Partial is true when the measurement stopped at its bound, so Bytes is a
	// lower bound.
	Partial bool `json:"partial,omitempty"`
}
