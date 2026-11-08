package fleet

// ScratchUsage is how much a session's scratch directory holds (muster #419, #431):
// everything its runtime and its tools wrote on disk while it ran.
//
// # Why it is on the state
//
// A session that waits for a person for a day can sit on gigabytes of test
// fixtures, build output and tool caches in its own directory, and nothing said so
// until the disk was full. The directory is removed when the session is closed;
// until then it is only the size of it that is worth knowing, and it is a fact about
// the session, so it travels with the state a poller already reads.
//
// # What the figure is
//
// The sum of the sizes of the regular files under the session's whole directory, as
// the file system reports them: not the space they occupy, and a link is not
// followed. It is the whole directory because that is what closing the session
// removes (#431: a figure for the temporary files alone read 8.5 GiB for sessions
// whose directories held 49.2 GiB). Because it counts apparent size, it can
// overstate the space in use: copy-on-write clones and hard-linked files are counted
// at full size each time they appear.
//
// Bytes is measured, not estimated, and it is not live — a driver measures
// at most every few minutes, never on the request path — so AsOf says when, and a
// reader seeing a size that stopped moving can tell. Partial is true when the walk
// was cut short by its bound, in which case Bytes is a lower bound.
//
// Areas says where the bytes sit, so a reader can tell disposable output from what
// the session needs to keep running. A driver names its own areas; the figures in
// Areas add up to Bytes.
//
// A figure is reported for as long as the directory exists, a dead session's
// included: the directory outlives the process until the session is closed.
//
// Absent means no figure: the driver keeps no such directory for the session, the
// directory is gone, or it has not measured it yet. Never a zero size (§5.7). It is
// not a material change (MateriallyDiffers): it moves with every file written, so a
// feed that fired on it would storm.
type ScratchUsage struct {
	// Bytes is the total size of the regular files under the whole scratch
	// directory — the sum of Areas.
	Bytes int64 `json:"bytes"`
	// Areas is Bytes split by where the files sit, keyed by a name the driver
	// chooses (the opencode driver reports "tmp", "home" and "xdg", and "other"
	// for anything outside them, present only when it holds something). Absent
	// when the driver does not split the directory, or on a peer built before the
	// field.
	Areas map[string]int64 `json:"areas,omitempty"`
	// AsOf is when the directory was measured, not when this state was read.
	AsOf Timestamp `json:"asOf"`
	// Partial is true when the measurement stopped at its bound, so Bytes is a
	// lower bound.
	Partial bool `json:"partial,omitempty"`
}
