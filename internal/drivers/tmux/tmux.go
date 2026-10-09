// Package tmux implements driver.Driver over a terminal multiplexer
// running an interactive agent CLI. It is the first driver in this
// repository that actually does anything, and its purpose is stated in
// NOTES' sequencing: prove the interface can express everything the
// incumbent supervisor already does. Where it cannot, that is a finding
// about the interface, not a gap to paper over — see FINDINGS below.
//
// # Enumeration is one subprocess, not N
//
// driver.Driver.List's doc comment warns that a driver implementing List by
// looping per session "has reproduced the cost this interface exists to
// avoid". That cost is real and was measured here, on a host running 22
// concurrent sessions:
//
//	per-session capture loop (23 spawns) ... 119ms
//	single batched invocation (1 spawn) ...   18ms
//
// The multiplexer accepts a sequence of commands separated by a literal
// ";" argument in one invocation, so a full fleet view — metadata for every
// session plus a screen capture of each — costs exactly one process spawn
// regardless of session count. Cost then scales with output bytes rather
// than with process creation, which is the difference between ~5ms per
// session and ~0.15ms per session.
//
// # Pane text is untrusted input
//
// The batched captures arrive concatenated with no delimiter, so this
// driver interleaves a marker between them. The marker is a per-call nonce
// rather than a constant, because the text being delimited is written by an
// agent that can print anything at all — including a convincing forgery of
// whatever fixed delimiter a naive implementation would choose. The same
// nonce separates fields within a metadata row, since session names and
// working directories are user-controlled and may contain any printable
// character (the sessions this was developed against contain emoji).
//
// # FINDINGS: where the specification did not survive contact
//
//  1. §5.4 ("require consensus before destruction") is not implementable at
//     the signature §3 gives close(). The rule requires corroborating "at
//     least one independent attribute (working directory, start time,
//     name)" before destroying a session — but SessionRef carries only
//     machine, id and a human label, so a driver has nothing to corroborate
//     the live session *against*. It can read the session's current start
//     time; it cannot know which start time the req meant. See
//     Driver.Close, which implements the strongest form available at this
//     signature and documents the window it cannot close.
//
//  2. §4.3's SupportsResume and §10's idempotency retention are different
//     properties, and the spec treats them as one concern. Sessions here
//     genuinely survive a service restart — the multiplexer owns them, not
//     this process — so SupportsResume is true. The idempotency key store
//     does not survive, because it lives in this process's memory. A req
//     that retries a create across a service restart therefore gets a
//     second session, which is precisely the §10 disaster, on a driver that
//     honestly declares SupportsResume: true. See idempotency below.
//
//  3. The multiplexer's "current command" field reports the process's
//     self-declared title, not its executable. The agent CLI rewrites its
//     title to its own version string, so the field returns "2_1_220" for a
//     process the OS calls "claude". It is never empty and never errors —
//     it is a confidently wrong answer of the wrong kind entirely, which is
//     §5.2's failure mode in a single field. This driver reads it as a
//     runtime version hint and never as an identity check.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
	"github.com/futurelastic/muster/internal/trustseed"
)

const (
	// DefaultRuntime is the runtime id this driver reports. It names both
	// halves deliberately: the multiplexer supplies the session substrate,
	// the CLI supplies the agent, and neither alone identifies what a
	// req is talking to.
	DefaultRuntime = fleet.RuntimeId("claude-code-tmux")

	// defaultDeadlineMs bounds any single call (§4.4). Local subprocess
	// work measured in single-digit milliseconds; the original 5000 was
	// three orders of magnitude of headroom on that basis, chosen so that a
	// genuinely wedged multiplexer surfaces as unreachable in bounded time
	// rather than never.
	//
	// Raised to 30000 for muster#129: Discard's clear loop is now
	// content-derived (see clearPressMargin, maxClearPresses below) rather
	// than clock-bound, and a composer sized like #129's own field case
	// (~80 rows) legitimately needs on the order of maxClearPresses presses
	// at promptClearInterval apart — arithmetic that no longer fits under
	// the original bound at all, regardless of what "wedged" means. §4.4
	// ties the declared deadline to the whole driver, not to one verb, so
	// this is the one number every operation's "wedged" detection now
	// shares; a subprocess that is actually hung is still caught, just with
	// more slack than before, which is the correct trade once one verb's
	// legitimate worst case is this much larger than the rest.
	defaultDeadlineMs = 30000

	// defaultCaptureLines is how much scrollback the classifier receives
	// per session. The classifier reads only the tail, but the tail must
	// be tall enough to contain the composer fence plus the status line
	// above it.
	defaultCaptureLines = 24

	// captureChunkMaxArgs and captureChunkMaxBytes bound how many
	// sessions' worth of display-message/capture-pane pairs go into ONE
	// invocation of the batched enumeration (muster#141).
	//
	// The multiplexer's own client-to-server command channel refuses a
	// chained invocation outright once it gets big enough — not a timeout,
	// not this process's own ARG_MAX, but the multiplexer answering
	// "command too long" and producing no output at all. Measured against
	// a real (private, isolated) server, tmux 3.7, by bisection:
	//
	//	argc wall:  995 args OK, 1007 args fails  (~83 sessions OK, ~84 fails)
	//	byte wall:  ~16.3-16.4KB of joined argv fails, independent of argc
	//
	// The argc wall is the one that fires in practice — session identifiers
	// and this driver's marker are short, so the byte wall is rarely
	// reached before the argc wall is. captureChunkMaxArgs sits ~10% under
	// the measured 995-safe / 1007-fails boundary — deliberately not a huge
	// margin, both because a fleet's realistic single-machine size can
	// itself run into the 80s (muster#141's own follow-up: the SAME
	// affected machine went clean carrying 81 sessions, just below this
	// cliff, without needing to drop anywhere near a healthy peer's 22 — a
	// third data point that lands almost exactly where this bisection put
	// the wall) and because a much smaller cap would mean chunking, and its
	// extra spawns, on fleet sizes that do not need it at all.
	// captureChunkMaxBytes keeps its wider margin under the independent
	// ~16.3-16.4KB byte wall, since that one is not the cap expected to
	// bind for ordinary short identifiers — it exists as a second, cheaper
	// insurance policy for the day a pane id or cwd is unusually long. The
	// exact thresholds are this multiplexer BUILD's, not a documented
	// contract, so a peer machine, a different OS, or a future upgrade
	// could move either wall without warning; neither cap claims to be
	// exact, only safely inside what was actually measured.
	//
	// Below either cap, a fleet still costs exactly one spawn — the cost
	// model docs/internals.md measured (22 sessions, 18ms) is unchanged.
	// Above it, enumerate() issues additional invocations rather than
	// letting the whole batch come back empty and misclassifying every
	// session in it as a driver malfunction, which is what muster#141
	// reported: 85 sessions (1020 args) past the wall, 22 sessions
	// (264 args) nowhere near it.
	captureChunkMaxArgs  = 900
	captureChunkMaxBytes = 14 * 1024

	// promptClearWindow bounds the wait for an answered prompt to disappear.
	promptClearWindow   = 3 * time.Second
	promptClearInterval = 200 * time.Millisecond

	// stallPresses bounds how many CONSECUTIVE identical captures Discard's
	// clear loop tolerates once it has already seen the composer move at
	// least once (#87). Before any movement, "unchanged" is not yet
	// evidence of anything — a pane that has simply not redrawn — so the
	// loop still spends its whole content-derived press budget there (see
	// clearPressMargin, maxClearPresses below; muster#129 replaced
	// what used to be a flat clock here). AFTER movement, an unchanged
	// capture is a press that did nothing, and repeating it for the rest of
	// the budget is not buying more evidence, it is more destructive
	// keystrokes aimed at text nobody has re-read. Three is enough to
	// distinguish "stopped" from "one slow repaint" without costing the
	// caller most of the budget finding out.
	stallPresses = 3

	// clearPressMargin is how many presses beyond composerVisualLines'
	// count a clear pass is given before the composer having never moved at
	// all counts as evidence rather than as "the pane has not repainted
	// yet" (muster#129). composerVisualLines is a count of what is
	// ON SCREEN right now, not a guarantee that C-u maps onto it one for
	// one — this margin is the acknowledgment that the mapping is measured,
	// not proven exact (see composerVisualLines' own doc comment), without
	// being large enough to matter for how long a genuinely stuck composer
	// takes to be reported as such.
	clearPressMargin = 5

	// maxClearPresses is the hard ceiling on how many C-u presses one clear
	// pass will ever spend, regardless of how large composerVisualLines
	// reports the composer to be. #129 is explicit that a human paste is
	// not bounded by what the API's own input cap would allow, so an
	// expectation derived from content has no natural ceiling of its own —
	// this is the bound this driver still needs regardless (§4.4: "a
	// driver that can block without a bound is a specification violation").
	// Sized comfortably above #129's own field case (~80 rows) so that case
	// is not the thing this limits; a composer that genuinely exceeds it
	// still gets an honest "made progress, ran out of budget" report
	// (discardIncomplete's damaged branch) rather than either a silent
	// truncation or an unbounded loop — the same outcome a composer that
	// stalls for any other reason already produces, so a caller does not
	// need to know which of the two happened to react correctly: re-read
	// and, if there is more to clear, ask again.
	maxClearPresses = 120

	// composerLineEndKey positions the cursor at the end of the current row
	// before clearComposer sends C-u (muster#138). C-u (unix-line-
	// discard) kills from the cursor back to the start of the line — it is
	// only guaranteed to remove the WHOLE row when the cursor is already
	// sitting after all of that row's content, an assumption
	// composerCursorRowBlank's row-blankness proxy does not actually
	// establish (see clearComposer's own doc comment). Named as its own
	// constant, not inlined as a literal "End", so a field measurement that
	// finds this TUI does not bind End can swap it for "C-e" in one place.
	composerLineEndKey = "End"

	// sweepMargin/maxSweepBackspaces/sweepBatchSize size clearComposerSweep
	// (muster#136), the Force escape hatch reachable only once the
	// ordinary row-budgeted pass has already been proven futile. The unit
	// here is CHARACTERS, not rows — this mechanism presses Backspace one
	// character at a time rather than clearing a structural row per press —
	// so #129's content-derived sizing argument is applied at the finer
	// grain this mechanism actually operates in.
	sweepMargin        = 8
	maxSweepBackspaces = 2000
	sweepBatchSize     = 40

	// startingWindow is how long a session with no visible interface is
	// given the benefit of the doubt. The runtime takes tens of seconds to
	// paint; beyond this, silence means something other than booting.
	startingWindow = 90 * time.Second

	// unsentAgeWorthMentioning is when a composer holding text stops looking
	// like someone typing. Below it the age is noise; above it, it is the
	// whole story.
	unsentAgeWorthMentioning = 10 * time.Minute

	// submitConfirmWindow bounds the wait for delivered text to render before
	// it is submitted. Generous on purpose: a slow render and a stuck pane
	// look identical over a short budget, and failing early strands the text.
	submitConfirmWindow   = 4 * time.Second
	submitConfirmInterval = 150 * time.Millisecond

	// sendReceptiveWindow bounds Send's wait for the runtime to be able to
	// receive input. Deliberately SHORT: §4.4 caps a call at the driver's
	// declared deadline, so this covers the race, not a startup. Beyond it
	// Send refuses and says what to wait for.
	sendReceptiveWindow   = 2 * time.Second
	sendReceptiveInterval = 200 * time.Millisecond

	// promptPollInterval is how often readiness is checked. This is not the
	// polling §5.5 forbids: that rule is about callers learning of state
	// changes, and this is one driver waiting for a process it just started.
	promptPollInterval = 1500 * time.Millisecond
	// sessionGoneConfirmations is how many CONSECUTIVE polls must find a
	// session absent from the enumeration before settleNewSession treats it
	// as gone rather than as a listing race. A single miss is not enough
	// evidence on its own — "a pane can vanish between listing and capture"
	// is already a documented, transient shape elsewhere in this file — so
	// this asks for two in a row (muster #125's own bound: the session's
	// own lifetime, not a guessed duration) before giving up on delivery.
	sessionGoneConfirmations = 2

	// defaultIdempotencyRetention is how long a create key is honoured
	// (§10: "retention must outlive the caller's retry window").
	defaultIdempotencyRetention = 30 * time.Minute

	// strandedRetention is how long a stranded-delivery record (#11) is
	// honoured after the delivery that produced it. Same value as
	// defaultIdempotencyRetention, named separately because it answers a
	// different question — how long a stuck delivery is still worth
	// finishing on a caller's behalf, not how long a retry window is
	// honoured — and reusing the number is a deliberate consistency with
	// §10's already-argued "must outlive a service restart," not a
	// placeholder. Without an expiry, a durable record eventually matches
	// text a human typed that happens to be identical — precisely what
	// strandedMatches's exact-match rule exists to exclude.
	strandedRetention = 30 * time.Minute

	// futileClearRetention is how long a "this exact residue would not
	// move" record (#87) is trusted. Same reasoning as strandedRetention:
	// kept forever it stops being evidence about a composer the caller can
	// still see, since anything could have happened to the pane by then.
	futileClearRetention = 30 * time.Minute

	// deliveryMarkRetention is how long a delivery mark (#111) is trusted as
	// the denominator for `turns`. Deliberately much longer than
	// strandedRetention: a stranded-delivery record stops being useful once
	// the delivery it describes could no longer plausibly still be pending,
	// but `turns` is useful for the ENTIRE life of a dispatched worker — a
	// caller may reasonably check back hours later. Swept the same way, on
	// the same "kept forever it stops being evidence about anything in
	// particular" reasoning, just on a longer clock.
	deliveryMarkRetention = 24 * time.Hour
)

// ErrAmbiguousTarget is returned by a destructive operation whose target
// could not be corroborated (§5.4). It is deliberately not
// driver.ErrUnsupported: the driver supports the operation, and is refusing
// this particular invocation because it cannot establish that the session
// it would destroy is the session the req meant.
// ErrAmbiguousTarget wraps the package-level sentinel so callers can match on
// either, and so the service can map it to a wire kind without importing this
// driver.
var ErrAmbiguousTarget = fmt.Errorf("tmux: refusing a destructive operation: %w", fleet.ErrAmbiguousTarget)

// ErrNotFound is returned when no session matches a ref.
var ErrNotFound = errors.New("tmux: no such session")

// execFunc runs the multiplexer binary and returns its stdout. Injected so
// tests can drive the driver without a live multiplexer.
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// CommandBuilder produces the argv the multiplexer should run for a new
// session. contextFile is a path to the caller's context, already written
// to disk — it is passed by path and must never be inlined into the
// returned argv (§5.3). An empty contextFile means the req supplied
// none.
//
// This indirection keeps the multiplexer mechanics separate from the
// specifics of any one agent CLI: swapping the CLI is a new builder, not a
// new driver.
type CommandBuilder func(spec fleet.SessionSpec, contextFile string) []string

// Driver runs sessions as multiplexer sessions on the local machine.
//
// The zero value is not usable; use New.
type Driver struct {
	machine      fleet.MachineId
	runtime      fleet.RuntimeId
	bin          string
	deadline     time.Duration
	captureLines int
	build        CommandBuilder
	shell        string
	bareExec     bool
	run          execFunc
	dial         ctlDialer
	now          func() time.Time
	nonce        func() string

	store   *state.Store
	idemErr error

	// gateRemoteControlInput is #272's setting: when true, `input` delivers
	// /rc and /remote-control only for a caller holding the remote-control or
	// human-relay grant. Off by default for one release (the behaviour before
	// #272), then the default flips — see WithRemoteControlInputGate.
	gateRemoteControlInput bool

	mu sync.Mutex
	// observed is this driver's most recent sighting of each session id,
	// keyed by id. It is what Close corroborates against (§5.4) and what
	// startup reconciliation adopts into (§12).
	observed map[string]observation
	// idem is the durable idempotency table (§10). Backed by a file when a
	// state store is configured; in-memory otherwise, which is honest for a
	// throwaway instance and was the defect (D5) for a real one.
	idem      *idemStore
	retention time.Duration

	// quota is the account-level block (§2.3's QuotaBlock), remembered
	// because it outlives the screen that announced it and survives a
	// restart — a weekly limit measured on this fleet had four days to run,
	// and the service is deployed by restarting it.
	quota *fleet.QuotaBlock
	// quotaSinceObserved is true when quota.Since came from the runtime's
	// own record of the refusal (#56) rather than from this driver's first
	// sighting of the notice on screen. Not part of fleet.QuotaBlock's wire
	// shape — §2.3 documents Since as a timestamp for humans and callers to
	// read, not a provenance channel, and the honest label belongs in
	// SessionState.Evidence (quotaBlockedState) the same way every other
	// "how do we know this" note in this driver already lives in prose
	// rather than in a new structured field.
	quotaSinceObserved bool

	// stranded remembers, per session, text this driver delivered and could
	// not confirm — the record a resume is checked against.
	//
	// Durable when a state store is configured (#11): a composer holding
	// unsent text survives a multiplexer restart on its own, but until now
	// the driver's own memory of having put it there did not survive a
	// SERVICE restart — so resumeIfStranded, the one door out of §2.4's
	// busy-composer refusal, stopped working on exactly the deploys it
	// exists to survive. See noteStranded/strandedMatches for the
	// corroboration (§5.4: id + cwd, not id alone) and strandedRetention
	// for the lifetime a durable record needs that an in-memory one never
	// did.
	stranded map[string]strandedRecord
	// tombstones are what remains of stranded records that lapsed or were
	// replaced: the draft rule's proof that composer text is this driver's
	// own after the live record is gone (draftrule.go, #180 M2).
	tombstones map[string][]strandedTombstone

	// unconfirmed is the cross-path ledger (#184, route.go): per session, inbox
	// writes that put bytes on the socket and could not be confirmed. It is the
	// reason a retry of an inbox `unknown` cannot reach the terminal path with
	// the same text. Persisted beside stranded, for the same reason: a restart
	// must not turn "written, unconfirmed" back into "never sent".
	unconfirmed map[string][]unconfirmedEntry
	// inboxWindow overrides inboxConfirmWindow. Tests only.
	inboxWindow time.Duration

	// delivered remembers, per session, the most recent delivery THIS DRIVER
	// made into that session's composer — the denominator muster #111's
	// `turns` is counted relative to. Written once, at the moment Send's own
	// paste-buffer call succeeds (before Submit is even checked), so every
	// downstream outcome of that delivery — queued, stranded, confirmed —
	// shares one mark. A resume (opts.ResumeIfStranded) does NOT write a new
	// one: it finishes the SAME delivery, so the denominator must not move.
	//
	// Durable when a state store is configured, same reasoning as stranded:
	// a dispatched worker is meant to be checked on across a service
	// restart, and `turns` going silently absent on every session the moment
	// this machine redeploys would defeat the field's own purpose. See
	// noteDelivery/deliveryMarkFor and deliveryMarkRetention.
	delivered map[string]deliveryMark

	// resumeIntents remembers, per session, the conversation id a create
	// asked the runtime to resume — the durable note #72 needs to say
	// whether that was honoured, once the session's own conversation
	// resolves. Same shape as stranded, for the same reasons: durable when
	// a state store is configured, in memory otherwise, keyed on session
	// id with cwd carried for corroboration (§5.4). See resumeintent.go.
	resumeIntents map[string]resumeIntentRecord

	// conversationIntents is resumeIntents' mirror for the opposite request:
	// the conversation id a create asked the runtime to START (muster
	// #224), so a later List can tell whether the runtime's own record
	// agrees with it. Same shape, same reasons, same corroboration. See
	// conversationintent.go.
	conversationIntents map[string]conversationIntentRecord

	// createRecords remembers, per session, what a create asked to pin, ask
	// for a runtime surface, and carry as a prompt — the durable notes #84,
	// #85 and #86 need to answer what was APPLIED rather than only what was
	// REQUESTED, once each of those can be told. Same shape as
	// resumeIntents, for the same reasons. See createrecord.go.
	createRecords map[string]createRecord

	// controlRecords caches the last read of each session's remote-control
	// record entries (controlchannelstate.go, muster #269).
	controlRecords controlRecordCache

	// rcLocks serialises remote-control toggles per session (remotecontrol.go).
	rcLocks rcLocks

	// environments remembers what each created session's process received
	// (see environment.go). In memory only, for the reason stated on
	// Environment.
	environments map[string]fleet.SessionEnvironment

	// futile remembers, per session, that Discard's clear loop already
	// spent a full pass against a specific composer residue and produced
	// no movement at all (#87). In memory only, like environments: this is
	// transient evidence about one clear attempt, not a fact the service
	// owes a restart — a restart re-earns it in the time one ordinary pass
	// takes, exactly as a genuinely first-time-frozen composer would. See
	// noteFutile/futileClearAttempts/forgetFutile below.
	futile map[string]futileClear

	// counters is this driver's self-observability registry — see
	// counters.go. Its own mutex, not d.mu: nothing about a count is
	// otherwise related to session state, and sharing a lock would only
	// make counting something contend with it for no reason.
	counters counterSet

	// exitedMu guards exitedPending — muster #235: what reapExited
	// captured about a session's own process exit, waiting for DrainExits to
	// hand it to the service. Its own mutex, not d.mu, for the same reason
	// counters has its own: nothing else here needs the rest of the driver's
	// state, and a slow drain must never contend with an in-flight List.
	exitedMu      sync.Mutex
	exitedPending []driver.CapturedExit

	// conversations locates the runtime's own record of each session — see
	// conversation.go. Nil until a record root is configured, and nil is
	// what makes a listing report nothing at all rather than reporting that
	// no record was found.
	conversations *conversationStore

	// credentialPath is the runtime's own local credential store, stat'ed to
	// answer #12 (SessionState.CredentialGeneration, EventMachineAccount).
	// Empty means unconfigured — the honest default for a constructor a
	// test, a sandbox or another program can call — and every session then
	// reports the field absent rather than a guessed value. See
	// WithCredentialPath.
	//
	// Unlike quota this needs no field alongside it to remember a value
	// across reads: a file's modification time does not evaporate the way a
	// scrolled-away screen notice does, so the filesystem already holds the
	// fact and a cached copy in this struct would only be a second,
	// potentially stale one.
	credentialPath string

	// trustSeed pre-answers the runtime's folder-trust question for every
	// directory under a configured root — see internal/trustseed and #47.
	// Nil means unconfigured, the same off-by-default contract as
	// credentialPath: a driver built for a test never touches a real state
	// file merely because it was constructed. See WithTrustSeed.
	trustSeed *trustseed.Seeder

	// sessionEnv is this machine's declared identity for its sessions —
	// muster issue #94. Nil/empty means unconfigured, the same
	// off-by-default contract as credentialPath and trustSeed: a driver
	// built for a test never merges configuration into a caller's env
	// merely because it was constructed. See WithSessionEnv and
	// sessionenv.go's provisionSessionEnv.
	sessionEnv []SessionEnvEntry

	// psBin and psRun are muster #116's own exec seam, deliberately
	// separate from bin/run rather than reusing them. Those name and run the
	// multiplexer specifically (execFunc's own doc comment); psRun queries
	// the OS process table for a PID this driver already resolved from the
	// multiplexer, an unrelated external program with its own argv shape. A
	// shared field would make a test double built for one silently answer
	// for the other. See processidentity.go.
	psBin string
	psRun execFunc

	// inboxResolver and inboxDial are muster #119's own seam. Nil
	// means unconfigured — the same off-by-default contract as
	// credentialPath and trustSeed: a driver built for a test never
	// attempts an inbox delivery merely because it was constructed, and
	// Send behaves exactly as it did before #119 until a composition root
	// opts in. See inbox.go.
	inboxResolver InboxResolver
	inboxDial     inboxDialFunc

	// counterSources are counts kept OUTSIDE this driver by something the
	// composition root wired into it — muster #163's resolver index
	// counters are the first. Merged into Counters under their own names.
	// Nil means none, the same off-by-default contract as every seam above.
	// See WithCounterSource.
	counterSources []func() map[string]int64

	// composerLocks is terminal-path-v2's per-session serialisation (D4) —
	// see terminalpath2_lock.go. Zero value is ready to use, same as every
	// sync.Mutex-based field above.
	composerLocks composerLockTable

	// module is the delivery module Send hands a request to once every
	// decision about the request itself is made (#180). Nil means the
	// built-in terminal path (delivery_module.go).
	module delivery.Module

	// modCfg and mods are the optional external delivery modules (#185). Nil
	// means none is enabled — the default, and every path then behaves exactly
	// as it did before the field existed. See modulelane.go.
	modCfg *ModulesConfig
	mods   *moduleHost

	// processSessionsRoot is terminal-path-v2's second identity source
	// (item c / D6): the directory the runtime writes one `<pid>.json` file
	// into per running process, carrying that process's own sessionId. It is
	// read for a session's conversation whenever one is looked up (#182 —
	// the ~18/75 "resumed sessions" gap round-1 measured: a session whose
	// conversation began before it did is not findable by name), and again
	// by the transcript path to confirm a delivery. Empty means unconfigured, the same off-by-default
	// contract as conversations/credentialPath/trustSeed above: a driver
	// built for a test never reads a real `~/.claude/sessions` directory
	// merely because it was constructed. See terminalpath2_transcript.go
	// and WithProcessSessionsRoot.
	processSessionsRoot string
}

type observation struct {
	created time.Time
	cwd     string
	at      time.Time

	// status and statusSince implement §8's "`since` is the time the status
	// was first observed to hold, not the time it began".
	//
	// This is what separates a wedged pane from an operator mid-thought. A
	// sibling project measured a session holding the same unsent line for
	// fourteen hours while its supervisor's veto — "an operator has text
	// pending, do not evict" — stayed correct policy applied to a premise
	// that had stopped being true. The veto assumes a human will come back;
	// on a wedged pane none can, because typing does nothing.
	//
	// Nothing here probes the pane to find out. The discriminator that
	// project used was to type a character and see whether it appeared,
	// which is not something to do to a live session. Duration is the same
	// signal read passively: text unchanged for hours is not a sentence
	// somebody is still composing.
	status      fleet.Status
	statusSince time.Time
	// sinceRestored marks a statusSince that came from disk rather than from
	// an observation this instance made. It travels with the observation so
	// the provenance is not lost the moment the value is cached in memory.
	sinceRestored bool

	// gapSince is non-zero while the LATEST read of this pane was a driver
	// malfunction (a capture that returned nothing), and is when the first of
	// the consecutive failed reads happened (#278).
	//
	// Such a read observed nothing, so it must not overwrite the last real
	// observation: status, statusSince, digest and at above keep describing
	// the last time this driver actually looked. Before this field the failed
	// read stored `unknown`, and the next good read found `idle` against a
	// prior `unknown` and restarted `since` at now. One cancelled capture
	// therefore made every idle session in the chunk read as active a minute
	// ago.
	gapSince time.Time

	// digest fingerprints the screen this observation classified, so the
	// next one can tell "unchanged" from "changed" without keeping the pane
	// text. See classify.go's resolveAmbiguity: an unchanged screen is what
	// settles "idle or a turn that has not painted yet", which is otherwise
	// the largest source of unknown in a real fleet.
	digest string

	// digestSince is when this pane was first seen showing digest, carried
	// forward across every read that finds the same digest again (#159).
	//
	// `at` cannot stand in for it, and for a while it did: `at` is the time of
	// the LAST read, and every State or List call overwrites it. The
	// resolutions that ask "has this screen held for spinnerPaintGrace" were
	// therefore really asking "how long since anybody last looked" — so with a
	// poller reading every second or so, one unchanged screen answered
	// waiting_input to a read that happened to land 2s after the previous one
	// and unknown to one that landed sooner. Measured on a multi-question
	// dialog's review screen: roughly one read in three, same screenDigest
	// throughout. A screen's age is a property of the screen, not of the
	// reader's cadence.
	digestSince time.Time
}

// Option configures a Driver.
type Option func(*Driver)

// WithBinary sets the multiplexer executable. Default "tmux".
func WithBinary(path string) Option { return func(d *Driver) { d.bin = path } }

// WithDeadline overrides the declared per-call deadline (§4.4).
func WithDeadline(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.deadline = dur
		}
	}
}

// WithCaptureLines sets how much scrollback per session is fed to the
// classifier.
func WithCaptureLines(n int) Option {
	return func(d *Driver) {
		if n > 0 {
			d.captureLines = n
		}
	}
}

// WithCommandBuilder replaces the default agent CLI invocation.
func WithCommandBuilder(b CommandBuilder) Option {
	return func(d *Driver) {
		if b != nil {
			d.build = b
		}
	}
}

// WithLoginShell sets the interpreter a created session's argv is wrapped in.
// Default: $SHELL, or a platform default when the process manager does not
// export one — which is the common case, not the exception.
func WithLoginShell(path string) Option {
	return func(d *Driver) {
		if path != "" {
			d.shell = path
		}
	}
}

// WithBareExec runs the agent directly, with no shell in front of it.
//
// This is the OLD behaviour and it is not the default, because it is what made
// a created session second-class: with no shell there is no startup file, and
// with no startup file there are no credentials, so the agent starts perfectly
// and fails at its first tool call. Available because a substrate whose agent
// needs no such environment should not pay for an interactive shell it does not
// need — but a caller reaching for it is opting out of parity, and should know
// that is what it is.
func WithBareExec() Option { return func(d *Driver) { d.bareExec = true } }

// WithRecordRoot points this driver at the runtime's own conversation record
// store, enabling the lookup that fills Session.Conversation.
//
// # Why this is off by default
//
// A driver constructed without it reports NOTHING about conversations — the
// field stays absent, meaning nobody looked — and that is the honest default
// for a constructor a test, a sandbox or another program can call. A default
// that read a real user's record store merely because a Driver was constructed
// would make an unconfigured process go looking through somebody's
// conversations, and would make every test's answer depend on the machine it
// ran on.
//
// The composition root supplies it, the same way it supplies the state store.
// An empty path disables the lookup again, explicitly.
func WithRecordRoot(path string) Option {
	return func(d *Driver) {
		if path == "" {
			d.conversations = nil
			return
		}
		d.conversations = newConversationStore(path)
	}
}

// WithProcessSessionsRoot points this driver at the runtime's own per-process
// identity directory (one `<pid>.json` file per running process, each
// carrying that process's own `sessionId`, `procStart` and `cwd`) — the
// second identity source: it identifies a session's conversation when the
// name-based lookup of d.conversations (WithRecordRoot) cannot, or disagrees
// (#182, conversationprocess.go), and terminal-path-v2's transcript-based
// submit confirmation reads it too. See terminalpath2_transcript.go.
//
// Same off-by-default contract as WithRecordRoot and every seam beside it:
// a driver constructed without this reads nothing from a real
// `~/.claude/sessions` directory, and an empty path disables it again,
// explicitly.
func WithProcessSessionsRoot(path string) Option {
	return func(d *Driver) { d.processSessionsRoot = path }
}

// WithCredentialPath points this driver at the runtime's own local
// credential store, enabling #12: a session's CredentialGeneration and the
// machine.account event both come from stat'ing this one path.
//
// Off by default for the same reason as WithRecordRoot — a driver
// constructed for a test or a sandbox must not go stat'ing a real file
// merely because it was built. The composition root supplies a real path;
// an empty one disables the feature again, explicitly, and every session
// then reports CredentialGeneration absent rather than a guessed value
// (§5.7).
func WithCredentialPath(path string) Option {
	return func(d *Driver) { d.credentialPath = path }
}

// WithTrustSeed enables #47's directory-trust seeding: statePath is the
// runtime's own state file (in practice, the same path WithCredentialPath
// points at — two options rather than one reused field, so credential
// generation stays a bare stat with no read of the file's contents, exactly
// as its own doc comment promises, regardless of whether this feature is
// on), home is the operator's home directory, and roots are the configured
// roots seeding is scoped to — machine-local configuration, like
// FLEET_PEERS' addresses, never committed to this repository.
//
// Off by default for the same reason as WithRecordRoot and
// WithCredentialPath: an empty statePath or a nil/empty roots list leaves
// trustSeed nil, and every method on internal/trustseed.Seeder is a no-op on
// a nil receiver, so Create never has to branch on whether this was
// configured.
func WithTrustSeed(statePath, home string, roots []string) Option {
	return func(d *Driver) { d.trustSeed = trustseed.New(statePath, home, roots) }
}

// WithSessionEnv declares this machine's identity for its sessions —
// muster issue #94. entries is expected to have already passed
// ValidateSessionEnv; this option does no validation of its own; the same
// division main.go already keeps for TrustRoots (validated once at startup,
// wired here without re-checking).
//
// Off by default for the same reason as WithRecordRoot, WithCredentialPath
// and WithTrustSeed: a driver built for a test must not merge configuration
// into a caller's env merely because it was constructed. An empty or nil
// list leaves provisionSessionEnv a no-op (see sessionenv.go).
func WithSessionEnv(entries []SessionEnvEntry) Option {
	return func(d *Driver) { d.sessionEnv = entries }
}

// TrustSeedResult passes through internal/trustseed.Result so a caller
// outside this package (cmd/muster's startup-and-interval maintainer)
// never has to import internal/trustseed itself.
type TrustSeedResult = trustseed.Result

// SeedTrustRoots runs one pass of #47's trust seeding — see
// internal/trustseed.Seeder.SeedAll. Meant to be called once at startup and
// again on an interval; a Driver with WithTrustSeed unconfigured returns a
// zero Result and a nil error, doing nothing.
func (d *Driver) SeedTrustRoots() (TrustSeedResult, error) {
	return d.trustSeed.SeedAll()
}

// withExec injects a fake multiplexer. Unexported: tests only.
func withExec(f execFunc) Option { return func(d *Driver) { d.run = f } }

// WithPSBinary sets the process-table query executable muster #116's
// process-identity resolution shells out to. Default "/bin/ps" — an
// absolute path, not a bare name, for the reason session-identity.md's
// "Two traps this feature inherits" section documents for this driver's own
// shell-outs: a created session's login-shell wrap gives it a real PATH, but
// this call runs outside that wrap, on the same clean four-entry search path
// as everything else this daemon shells out on directly.
func WithPSBinary(path string) Option {
	return func(d *Driver) {
		if path != "" {
			d.psBin = path
		}
	}
}

// withPSExec injects a fake process-table query. Unexported: tests only —
// the same shape as withExec, and deliberately not the same field (see
// Driver.psRun's own doc comment).
func withPSExec(f execFunc) Option { return func(d *Driver) { d.psRun = f } }

// withClock and withNonce make tests deterministic.
func withClock(f func() time.Time) func(*Driver) { return func(d *Driver) { d.now = f } }
func withNonce(f func() string) func(*Driver)    { return func(d *Driver) { d.nonce = f } }

// New builds a Driver for one machine.
func New(machine fleet.MachineId, opts ...Option) *Driver {
	d := &Driver{
		machine:      machine,
		runtime:      DefaultRuntime,
		bin:          "tmux",
		deadline:     defaultDeadlineMs * time.Millisecond,
		captureLines: defaultCaptureLines,
		build:        claudeCodeCommand,
		run:          runReal,
		dial:         dialReal,
		psBin:        "/bin/ps",
		psRun:        runReal,
		inboxDial:    dialInboxReal,
		now:          time.Now,
		nonce:        randomNonce,
		observed:     map[string]observation{},
		retention:    defaultIdempotencyRetention,
	}
	for _, o := range opts {
		o(d)
	}
	// Constructed after options so a configured state store and retention
	// are both in effect. An unreadable table is fatal rather than silently
	// discarded: losing keys quietly is how §10's disaster arrives dressed
	// as a clean start.
	idem, err := newIdemStore(d.store, d.retention, d.now)
	if err != nil {
		d.idemErr = err
		idem, _ = newIdemStore(nil, d.retention, d.now)
	}
	d.idem = idem
	d.loadQuota()
	d.loadStranded()
	d.loadDelivery()
	d.loadResumeIntents()
	d.loadConversationIntents()
	d.loadCreateRecords()
	d.initModules()
	return d
}

// WithState makes this driver remember idempotency keys across a restart
// (§10, defect D5).
func WithState(st *state.Store) Option { return func(d *Driver) { d.store = st } }

// StateError reports a failure to load durable state, if any. A caller that
// ignores it gets a working driver with an empty key table, which is the
// behaviour that made D5 a defect — so cmd surfaces it at startup.
func (d *Driver) StateError() error { return d.idemErr }

var _ driver.Driver = (*Driver)(nil)

func runReal(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// noServerRunning reports whether err is the multiplexer itself saying there
// is no server to ask — as opposed to the invocation failing (muster#157).
//
// Only a process that ran and exited counts: the stderr text is read from the
// *exec.ExitError that Output() fills in, never from an error string, so a
// wrapper error that merely quotes these words does not qualify. Two
// signatures, both measured against tmux 3.7c:
//
//   - no socket file at all (every session closed and the server exited):
//     "error connecting to <path> (No such file or directory)"
//   - a socket file left behind with nobody listening on it:
//     "no server running on <path>" (the wording older versions print for
//     the first case too)
//
// The "error connecting to" prefix is deliberately NOT enough on its own:
// the same prefix carries "(Permission denied)" and "(File name too long)",
// both measured, and both are a socket this driver could not read — a
// machine whose sessions are unknown, not a machine that has none.
func noServerRunning(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	for _, line := range strings.Split(string(exitErr.Stderr), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "no server running on ") {
			return true
		}
		if strings.HasPrefix(line, "error connecting to ") &&
			strings.HasSuffix(line, "(No such file or directory)") {
			return true
		}
	}
	return false
}

// Capabilities declares what this driver can and cannot do (§4.3).
//
// ObservesState is false and must stay false: every status this driver
// reports is inferred from a terminal screen (see classify.go). Setting it
// true would be the §5.6 violation the field exists to make visible.
//
// DeliversRawKeys is true. This driver owns a real pane (keys.go implements
// driver.KeySender against it) and produces the SessionState.ScreenDigest
// that corroborates each key. This is the one capability whose truth this
// driver's whole substrate is built from — a terminal multiplexer session IS
// a screen — so unlike ObservesState there is no substrate-fidelity reason
// to report anything but true here.
//
// ConfirmsDelivery is false, and for TWO reasons rather than the one
// originally written here.
//
// The first is a limit on observing an OUTCOME: the driver can see afterwards
// that the composer is empty, but "the composer is empty" does not distinguish
// "the agent received it" from "something else cleared it".
//
// The second is a limit on verifying its own ACTION, and it is the one this
// note used to omit. Send issues a submit keystroke and then returns without
// looking: the confirmation it performs happens BEFORE the submit and proves
// only that the text rendered. So the driver cannot say the submit registered
// either — not merely that it cannot see what came of it. Those are different
// claims, and collapsing them made `queued` read as stronger than it is.
//
// Respond does not have this gap: it calls promptCleared afterwards and
// downgrades to unknown. Send has no equivalent, which is why its receipt now
// names the submit as unverified rather than scoping the doubt to the agent.
//
// SupportsResume is true, and it is the one capability this substrate has
// outright: sessions belong to the multiplexer, not to this process, so
// they survive it being restarted, upgraded or killed. Note FINDINGS 2 —
// this is not the same as the idempotency store surviving.
// A note on fleet.Caller, which every operation below now takes: this is a
// LOCAL driver, so it has no peer to present credentials to and ignores
// Caller.Credential entirely. It must still never invent a Principal it was
// not handed — the audit trail §6 requires is only worth having if nothing
// in the chain manufactures an actor.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		ObservesState:   false,
		DeliversRawKeys: true,
		// This driver reads the runtime's own control-channel status label
		// off the pane footer (controlchannel.go). Declared rather than
		// assumed, so a nil ControlChannel is answerable: on this driver it
		// means the runtime rendered no label, not that nobody looked.
		ObservesControlChannel: true,
		// #194: this driver reads the runtime's permission-mode indicator off
		// the same footer region (permissionmode.go). Declared for the same
		// reason: an absent state.permissionMode on this driver means nothing
		// was readable at that moment (a dialog owns the screen), not that
		// nobody looked.
		ObservesPermissionMode: true,
		// #85: this driver latches Session.RuntimeSurface off the same
		// footer label ObservesControlChannel already reads, once
		// corroborated — see surface.go's runtimeSurfaceFor.
		ReportsRuntimeSurface: true,
		ConfirmsDelivery:      false,
		SupportsResume:        true,
		// #122: true only once a composition root has actually wired
		// WithInboxResolver — set once at construction, never mutated
		// after, so reading it here needs no lock, the same as
		// d.deadline just below. A nil resolver (every test in this
		// package that does not pass one, and any consumer that has not
		// wired one) reports false, matching sendViaInbox's own
		// first-line check (inbox.go) exactly rather than approximating it.
		DeliversToInbox: d.inboxResolver != nil,
		SupportsPin: fleet.PinSupport{
			Model:  true,
			Effort: true,
			Agent:  true,
		},
		// muster #269: a running session's remote control can be turned on and
		// off through the runtime's own slash command and its disconnect
		// dialog (remotecontrol.go). Both halves measured live.
		RemoteControl: &fleet.RemoteControlSupport{Toggle: true, Off: true},
		DeadlineMs:    d.deadline.Milliseconds(),
		// #185: the optional external delivery modules enabled here and how
		// each is wired; absent when none is.
		DeliveryModules: d.moduleStatuses(),
		// A local driver is describing itself, so this is observed by
		// definition — there is no network between the claim and its
		// subject.
		Source: fleet.CapabilitiesObserved,
	}
}

// Counters implements driver.CounterReporter. See counters.go's doc comment
// for this driver's own registry; #47's trust-seed counts (see
// internal/trustseed) are merged in under their own names when that feature
// is configured, rather than exposed through a second driver — one map, one
// reader, and the two registries' names do not collide because trustseed's
// are all prefixed "trust_seed.".
//
// Sources added with WithCounterSource are merged last, and never overwrite a
// name already present: a collision would silently fold two different facts
// into one count, so the driver's own number keeps the name and the source's
// is dropped. A source owns a prefix of its own precisely so that never
// happens.
func (d *Driver) Counters() map[string]int64 {
	out := d.counters.Snapshot()
	for k, v := range d.trustSeed.Counters() {
		out[k] = v
	}
	for _, src := range d.counterSources {
		for k, v := range src() {
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	return out
}

// WithCounterSource adds counts kept outside this driver to what Counters
// reports — muster #163. The first caller is the composition root's
// inbox resolver: an InboxResolver is a bare function, so the counts it keeps
// about the index it reads have no driver of their own to reach GET
// /v1/health through, and the resolver only ever runs inside this driver's
// send path anyway. src is called on every Counters read, must be safe to call
// concurrently, and should name everything under a prefix of its own (see
// Counters on collisions). A nil src is ignored.
func WithCounterSource(src func() map[string]int64) Option {
	return func(d *Driver) {
		if src != nil {
			d.counterSources = append(d.counterSources, src)
		}
	}
}

// WithRemoteControlInputGate turns #272's gate on or off: with it on, `input`
// refuses the runtime's /rc and /remote-control commands unless the caller
// holds the remote-control grant (or relays a human, the human-relay grant),
// so the grant means what it says. Off keeps the behaviour from before #272,
// where anyone holding send could turn remote control on by typing the command.
//
// Off is the default for ONE release, so a client still typing the command
// through `input` has a release to move to POST …/remote-control; the release
// after flips the default. The driver does not choose: the composition root
// passes what the operator set.
func WithRemoteControlInputGate(on bool) Option {
	return func(d *Driver) { d.gateRemoteControlInput = on }
}

// bounded applies this driver's declared deadline, or the caller's if the
// caller's is shorter (§4.4: "a req may supply a shorter deadline; never
// a longer one").
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.deadline)
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// managedSessionOption is the tmux user option Create sets on every session
// this driver starts (muster #236). reapDeadRows reads it back through
// enumerate's format string to tell a session this service created from one
// it merely happens to see on a shared multiplexer server — see Create's own
// comment on that sharing. A user option is scoped by name alone; any value
// this driver picks is safe as long as nothing else on the box sets the same
// name for an unrelated purpose, which a "colab-" prefix makes unlikely.
const managedSessionOption = "@colab-managed"

// paneRow is one session's metadata as returned by the batched enumeration.
type paneRow struct {
	session string
	paneID  string
	cwd     string
	pid     int
	created time.Time
	dead    bool
	// deadStatus is the pane's own process exit status (muster #235).
	// Meaningful only when dead is true — the runtime reports an empty/zero
	// value for a live pane, which parses indistinguishably from a real
	// status of 0, but nothing reads this field without checking dead first.
	deadStatus int
	// title is the process's self-declared title, NOT its executable name
	// — see FINDINGS 3. Carried as a version hint; never used to decide
	// what a session is.
	title string
	// managed is true only for a session this driver's own Create set
	// managedSessionOption on (muster #236). reapDeadRows reads this,
	// not dead alone, before acting — a pane can be dead AND belong to a
	// session this driver never started, one it shares its multiplexer
	// server with (Create's own comment on that sharing).
	managed bool
	// launchRC is what Create recorded about whether it put the remote-control
	// flag on the command line: "1", "0", or "" when unknown (launchRemoteControlOption).
	launchRC string
}

// enumerate performs the whole fleet read in one subprocess: one metadata
// listing followed by one screen capture per session, delimited by a
// per-call nonce.
func (d *Driver) enumerate(ctx context.Context) ([]paneRow, map[string]paneCapture, error) {
	nonce := d.nonce()
	sep := nonce + "F"
	mark := nonce + "P"

	// Restrict to the active pane of the active window: one row per
	// session. A session with several windows is still one session.
	const activeOnly = "#{&&:#{pane_active},#{window_active}}"
	format := strings.Join([]string{
		"#{session_name}", "#{pane_id}", "#{pane_current_path}",
		"#{pane_pid}", "#{session_created}", "#{pane_dead}",
		// muster #235: carried alongside pane_dead, in the SAME
		// invocation, rather than a second listing once a dead pane is
		// found — driver.Driver.List's constant-spawn contract holds
		// whether or not anything is dead this round.
		"#{pane_dead_status}",
		"#{pane_current_command}",
		// muster #236: the marker Create sets on every session this
		// driver starts (managedSessionOption). A pane option would not see
		// it — the option is set at the session, not the pane — but a
		// format expression follows tmux's own option inheritance
		// (session → window → pane) unless something more specific
		// overrides it, and nothing here ever sets a pane- or window-level
		// override, so this reads the session's value for every pane in it.
		"#{" + managedSessionOption + "}",
		// muster #269: whether the launch carried the remote-control flag
		// (launchRemoteControlOption) — the weakest evidence behind `off`.
		"#{" + launchRemoteControlOption + "}",
	}, sep)

	args := []string{"list-panes", "-a", "-f", activeOnly, "-F", format}

	// The listing must happen before the captures can be named, so this is
	// two subprocesses on a cold call and one thereafter... except it need
	// not be: the capture targets can be expressed as the same filter, so
	// a first cheap listing tells us the pane ids and the second call does
	// everything. Measured, the listing alone is ~8ms and the combined
	// call ~18ms; two calls total ~26ms for 22 sessions, still O(1) in
	// session count and still 4x cheaper than the per-session loop.
	//
	// Timed (muster#156) because a capture failure line that cannot say
	// how much of the call's budget the LISTING already spent cannot tell "the
	// capture was slow" from "the capture inherited almost nothing".
	listStart := time.Now()
	out, err := d.run(ctx, d.bin, args...)
	listWall := time.Since(listStart)
	if err != nil {
		// muster#157: a machine whose multiplexer has no server is a
		// machine with zero sessions — the normal resting state once its last
		// session closes — and that is a complete answer, not a failed one.
		// Reporting it unreachable turned every fleet-scope read incomplete
		// for as long as the machine stayed idle. Only the multiplexer's own
		// no-server messages qualify (see noServerRunning); anything else —
		// a missing binary, a socket it may not open, a deadline kill (a
		// killed process printed no such message) — is still a read that
		// did not happen. Deliberately no ctx.Err() guard: whether the
		// deadline has passed says nothing about what the multiplexer
		// already answered, and coupling the verdict to the clock would make
		// it depend on when it was asked rather than on what was said.
		if noServerRunning(err) {
			return nil, map[string]paneCapture{}, nil
		}
		return nil, nil, fmt.Errorf("enumerate: listing sessions: %w", err)
	}
	d.noteSlowInvocation("listing", listWall, 0)
	rows, err := parseRows(string(out), sep)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, map[string]paneCapture{}, nil
	}

	// One invocation per CHUNK, N captures per chunk, nonce-delimited.
	//
	// The marker carries each pane's INDEX, never its pane id, and that is
	// not a stylistic choice. display-message passes its argument through
	// strftime before printing it, so any "%" in the marker is consumed as
	// a (usually invalid) conversion specifier and silently vanishes —
	// and every pane identifier on this substrate begins with "%". A
	// marker built from a pane id therefore arrives corrupted, the
	// captures fail to associate with their sessions, and — the part that
	// makes this worth a comment rather than a fix — every session
	// classifies as "unknown" instead of erroring, because an absent
	// capture is indistinguishable from an empty one. It looks like a
	// working driver that cannot read screens.
	//
	// Chunking (muster#141) is why this is "per chunk" rather than "the
	// whole fleet, always" as an earlier version of this comment said: past
	// captureChunkMaxArgs/captureChunkMaxBytes worth of chained commands, the
	// multiplexer's OWN client-to-server channel refuses the invocation
	// outright ("command too long"), and that failure is total — nothing in
	// the batch comes back, so every session in it would misclassify as a
	// driver malfunction, not just the ones past the cap. See the constants'
	// doc comment for the measured thresholds. Each chunk is independent: a
	// wall crossed by chunk 2 does not take chunk 1's already-captured
	// screens down with it.
	//
	// Chunking bounds each invocation's SIZE; muster#156 is the other
	// wall, TIME. The whole call — listing, every chunk, and whatever the verb
	// does afterwards — shares one declared deadline (bounded), so a single
	// invocation that stalls used to spend all of it: the process was killed
	// at the call's own deadline, the chunk came back empty, and there was no
	// budget left to try again. Measured on three machines at 3 to 75 panes per
	// failure — including machines with only three or four panes, so pane count
	// is not the lever and a budget scaled by it would not have helped. So each
	// invocation now runs under a SLICE of what is left (captureSlice), never
	// more than half of it, and one empty chunk per enumeration whose slice
	// expired is retried with a fresh slice. Everything still derives from the
	// caller's context: no call runs longer than it did before.
	byIndex := make(map[string]paneCapture, len(rows))
	chunks := chunkPaneRows(rows)
	retryUnused := true
	for chunkIdx, chunkRows := range chunks {
		// Invocations still to run, this one included: the chunks from here
		// on, plus the retry while nobody has spent it.
		pending := len(chunks) - chunkIdx
		if retryUnused {
			pending++
		}
		// A pane can vanish in the gap between the listing above and this
		// capture — a session that ends while unobserved is ordinary churn
		// (reconcile.go treats it as exactly that), not a machine failure. The
		// multiplexer exits nonzero when ANY chained capture-pane target in this
		// one invocation is gone, and an earlier version of this call treated
		// that as fatal for the whole batch — discarding the screens of every
		// OTHER session in the same call, on a machine with any churn at all
		// (see #29).
		//
		// The err here is deliberately not returned. Go's Cmd.Output still hands
		// back whatever the process wrote to stdout before the failing
		// sub-command, and the association loop below already tolerates one
		// pane's capture going missing — an absent entry in byIndex classifies
		// that session "unknown" rather than aborting (see the
		// marker-corruption comment above). So a nonzero exit here is folded
		// into the same tolerance: keep whatever this invocation produced, and
		// let the caller count what is missing rather than throwing all of it
		// away. That tolerance is now per-chunk rather than fleet-wide, which
		// is strictly better: a #141-style wall in one chunk no longer costs
		// every OTHER chunk's already-good captures.
		//
		// The error is still not RETURNED — that contract is unchanged — but
		// muster#141 was invisible in the log for its entire life: the
		// only reason it was ever noticed is that dozens of sessions' state
		// timestamps stopped moving, and the only reason it was ever
		// confirmed to have RECOVERED is the same timestamps starting to
		// move again. Nothing in this driver's own log said a machine-wide
		// capture outage started, worsened, or ended. So a chunk that comes
		// back with nothing parseable at all — for a nonzero request — is
		// logged once, here, at the point where the information still
		// exists to say WHY: an exit error is the multiplexer explaining
		// itself, and its absence with equally empty output points at the
		// marker-corruption failure mode instead. Best-effort only; a
		// logging call that itself panics or blocks must never be how a
		// caller learns this driver is unavailable.
		//
		// muster#156 widened that line: it now carries the invocation's
		// wall time against the slice it was given, how much of the call's
		// budget was left, and how long the listing took. Without those, "the
		// multiplexer was slow" and "the budget was too small" cannot be told
		// apart, and that was the one question the first round of failures
		// could not answer. The #141 prefix is kept word for word so an
		// existing search of the log still finds these lines.
		chunkCaptures, att := d.captureChunk(ctx, chunkRows, mark, pending)
		if len(chunkCaptures) == 0 && len(chunkRows) > 0 {
			d.counters.incr(counterCaptureChunkFailed)
			detail := fmt.Sprintf("wall %v of %v slice (call budget left %v, listing took %v); "+
				"cause: %s; exit error: %v",
				roundMs(att.wall), roundMs(att.slice), roundMs(att.remaining), roundMs(listWall),
				att.cause, att.err)
			retryNote := ""
			switch {
			case att.cause != captureSliceExpired:
				// Retrying is only useful when this invocation hit its OWN
				// slice. If the caller has gone away or the call's budget is
				// used up, nobody is waiting for the answer. A multiplexer
				// exit is its own answer, and asking again gets the same one.
				retryNote = "not attempted (" + att.cause + " is not a slice expiry)"
			case !retryUnused:
				retryNote = "not attempted (this enumeration's one retry is already spent)"
			default:
				retryUnused = false
				retryCaptures, retry := d.captureChunk(ctx, chunkRows, mark, len(chunks)-chunkIdx)
				if len(retryCaptures) > 0 {
					d.counters.incr(counterCaptureRetryRecovered)
					log.Printf("tmux: batched capture came back empty for %d pane(s) "+
						"(chunk %d/%d of this enumeration), %s; retry recovered %d/%d pane(s) in %v",
						len(chunkRows), chunkIdx+1, len(chunks), detail,
						len(retryCaptures), len(chunkRows), roundMs(retry.wall))
					chunkCaptures = retryCaptures
					break
				}
				d.counters.incr(counterCaptureRetryFailed)
				retryNote = fmt.Sprintf("failed after %v of %v slice (cause: %s; exit error: %v)",
					roundMs(retry.wall), roundMs(retry.slice), retry.cause, retry.err)
			}
			if retryNote != "" {
				log.Printf("tmux: batched capture returned nothing parseable for %d pane(s) "+
					"(chunk %d/%d of this enumeration) — every session in this chunk will "+
					"classify as a driver malfunction, not as an observation; %s; retry: %s",
					len(chunkRows), chunkIdx+1, len(chunks), detail, retryNote)
			}
		} else if att.err == nil {
			d.noteSlowInvocation(fmt.Sprintf("capture chunk %d/%d", chunkIdx+1, len(chunks)),
				att.wall, len(chunkRows))
		}
		for k, v := range chunkCaptures {
			byIndex[k] = v
		}
	}
	captures := make(map[string]paneCapture, len(rows))
	for i, r := range rows {
		if c, ok := byIndex[strconv.Itoa(i)]; ok {
			captures[r.paneID] = c
		}
	}
	return rows, captures, nil
}

// Why a capture invocation came back with nothing, as far as this driver can
// tell. These are log vocabulary. Only captureSliceExpired changes behaviour:
// it is the one cause where running the invocation again can produce
// a different answer inside the same call.
const (
	// captureSliceExpired: the invocation hit its own slice while the call
	// still had budget. It was the multiplexer that was slow, not the
	// caller that gave up.
	captureSliceExpired = "slice-expired"
	// captureCallerCancelled: the caller's context was cancelled (a client
	// disconnected, a subscription closed). Nobody is waiting for the answer.
	captureCallerCancelled = "caller-cancelled"
	// captureBudgetExhausted: the call's whole declared deadline has run out.
	// Before muster#156 every stall ended in this cause, because
	// the capture ran under the call's full budget.
	captureBudgetExhausted = "call-budget-exhausted"
	// captureMultiplexerExit: the process exited on its own with an error —
	// the multiplexer answering, not the clock.
	captureMultiplexerExit = "multiplexer-exit"
	// captureNoOutput: exited cleanly and still produced nothing
	// parseable. This is the marker-corruption shape described in enumerate.
	captureNoOutput = "no-output"
)

// captureAttempt is what one batched capture invocation reports about
// itself, beyond its output.
type captureAttempt struct {
	wall      time.Duration // measured on the real clock, whatever d.now says
	slice     time.Duration // the budget this invocation was given
	remaining time.Duration // the call's budget left when it started
	err       error
	cause     string // one of the capture* causes; meaningful only when output was empty
}

// captureChunk runs ONE batched display-message/capture-pane invocation for
// chunkRows under a slice of the caller's remaining budget (captureSlice),
// and reports how it went. pending is the number of invocations still to run
// in this enumeration, this one included.
//
// The slice is a child of ctx, never a replacement for it: a caller's own
// deadline or cancellation still ends the invocation at once, and nothing
// here can make a call run longer than its declared deadline (§4.4).
func (d *Driver) captureChunk(ctx context.Context, chunkRows []indexedPaneRow, mark string, pending int) (map[string]paneCapture, captureAttempt) {
	capArgs := make([]string, 0, len(chunkRows)*14)
	for j, r := range chunkRows {
		if j > 0 {
			capArgs = append(capArgs, ";")
		}
		// The shape comes from classifyCaptureArgs, which is where the -e
		// rationale lives: the composer's placeholder is distinguishable from
		// typed input only by being rendered dim, and stripping colour here
		// would discard the one signal separating "nobody typed anything"
		// from "do not overwrite me".
		// The marker names the pane explicitly (-t) because it now expands a
		// format: without a target, display-message reads the CURRENT pane,
		// not the one about to be captured (muster#169).
		capArgs = append(capArgs, "display-message", "-t", r.paneID, "-p",
			mark+strconv.Itoa(r.index)+" "+paneHeightFormat, ";")
		capArgs = append(capArgs, classifyCaptureArgs(r.paneID, d.captureLines)...)
	}

	// Measured on d.now, the same clock bounded() set the deadline with.
	remaining := d.deadline
	if dl, ok := ctx.Deadline(); ok {
		remaining = dl.Sub(d.now())
	}
	att := captureAttempt{slice: captureSlice(remaining, pending), remaining: remaining}

	runCtx := ctx
	if att.slice > 0 {
		c, cancel := context.WithDeadline(ctx, d.now().Add(att.slice))
		defer cancel()
		runCtx = c
	}
	start := time.Now()
	out, err := d.run(runCtx, d.bin, capArgs...)
	att.wall = time.Since(start)
	att.err = err

	// The order matters. A child context is done whenever its parent is, so
	// the parent is checked first: only a child that expired under a parent
	// still live counts as this invocation's own slice running out.
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		att.cause = captureCallerCancelled
	case ctx.Err() != nil:
		att.cause = captureBudgetExhausted
	case runCtx.Err() != nil:
		att.cause = captureSliceExpired
	case err != nil:
		att.cause = captureMultiplexerExit
	default:
		att.cause = captureNoOutput
	}
	return splitCaptures(string(out), mark), att
}

// captureSlice is the budget ONE capture invocation may spend, given the
// call's remaining budget and the invocations still to run (this one
// included). One extra share is always held back, for the verb's own work
// after the enumeration (Send's paste and submit, Discard's clear loop). So
// no invocation ever gets more than half of what is left, and one stalled
// invocation can no longer take the whole call's deadline with it
// (muster#156).
//
// At the default 30 s with one chunk and the retry unspent, the first
// attempt gets 10 s. A healthy batched capture takes tens of milliseconds, so
// 10 s is still hundreds of times its normal cost. Any invocation that
// succeeds but runs slow enough to matter shows up in the log first (see
// noteSlowInvocation). This function is where to tune the slice if the log
// ever shows healthy captures getting killed.
func captureSlice(remaining time.Duration, pending int) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if pending < 1 {
		pending = 1
	}
	return remaining / time.Duration(pending+1)
}

// slowInvocationDivisor puts the "slow" line at 1/15 of the declared
// deadline: 2 s at the default 30 s. That is about a hundred times what a
// healthy invocation costs, and still far below any slice, so a slow success
// is logged well before it could turn into a kill.
const slowInvocationDivisor = 15

// noteSlowInvocation logs and counts one multiplexer invocation that
// succeeded but took longer than the slow line. Without this, the only wall
// times ever recorded come from failures, and failures alone cannot say how
// close healthy invocations run to the slice (muster#156).
// panes is 0 for an invocation that does not capture panes, such as the listing.
func (d *Driver) noteSlowInvocation(what string, wall time.Duration, panes int) {
	line := d.deadline / slowInvocationDivisor
	if wall <= line {
		return
	}
	d.counters.incr(counterEnumerateSlowInvocation)
	log.Printf("tmux: slow multiplexer invocation — %s took %v for %d pane(s) (slow line %v); "+
		"it succeeded, and is logged only so wall times are on record before the next failure",
		what, roundMs(wall), panes, roundMs(line))
}

func roundMs(x time.Duration) time.Duration { return x.Round(time.Millisecond) }

// indexedPaneRow pairs a paneRow with its position in the ORIGINAL rows
// slice, so a marker built while iterating one chunk still carries the
// index the caller's association loop expects — chunking must not
// renumber panes relative to the unchunked shape.
type indexedPaneRow struct {
	paneRow
	index int
}

// chunkPaneRows splits rows into groups small enough that the batched
// display-message/capture-pane invocation each group becomes never crosses
// the multiplexer's own command-length limits (muster#141; see
// captureChunkMaxArgs/captureChunkMaxBytes). A fleet under either cap comes
// back as a single chunk, preserving the one-spawn cost this file's package
// doc measured.
//
// The byte estimate here mirrors the shape enumerate() actually builds
// (display-message + its marker + capture-pane's fixed flags + the pane id)
// closely enough to budget correctly; it does not need to be exact, only
// conservative, and captureChunkMaxBytes already carries a ~2x safety
// margin over the measured wall for exactly this reason.
func chunkPaneRows(rows []paneRow) [][]indexedPaneRow {
	var chunks [][]indexedPaneRow
	var cur []indexedPaneRow
	curArgs, curBytes := 0, 0

	// Per-row argument count and a byte estimate, matching the shape built
	// in captureChunk(): [";"] "display-message" "-t" <paneID> "-p"
	// <marker+index+" #{pane_height}"> ";" "capture-pane" "-p" "-e" "-t"
	// <paneID> "-S" <lines> — 13 args for the first row in a chunk (no
	// leading ";"), 14 for every row after it. The pane id appears twice
	// since muster#169 targets the marker too.
	const fixedArgs = 13  // without the leading separator
	const fixedBytes = 80 // "display-message" "-t" "-p" " #{pane_height}" ";" "capture-pane" "-p" "-e" "-t" "-S" "-24" + separators, rounded up
	for i, r := range rows {
		rowArgs := fixedArgs
		rowBytes := fixedBytes + 2*len(r.paneID) + 12 /* room for the marker+index */
		if len(cur) > 0 {
			rowArgs++ // the leading ";"
			rowBytes += 2
		}
		if len(cur) > 0 && (curArgs+rowArgs > captureChunkMaxArgs || curBytes+rowBytes > captureChunkMaxBytes) {
			chunks = append(chunks, cur)
			cur = nil
			curArgs, curBytes = 0, 0
			rowArgs = fixedArgs // this row now starts a fresh chunk, no leading ";"
			rowBytes = fixedBytes + 2*len(r.paneID) + 12
		}
		cur = append(cur, indexedPaneRow{paneRow: r, index: i})
		curArgs += rowArgs
		curBytes += rowBytes
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

func parseRows(out, sep string) ([]paneRow, error) {
	var rows []paneRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, sep)
		// 9 fields before muster #269 added the launch option; accepted so a
		// listing from a multiplexer wrapper that predates it still parses.
		if len(f) != 9 && len(f) != 10 {
			return nil, fmt.Errorf("parseRows: expected 10 fields, got %d in %q", len(f), line)
		}
		launchRC := ""
		if len(f) == 10 {
			launchRC = strings.TrimSpace(f[9])
		}
		pid, _ := strconv.Atoi(f[3])
		createdUnix, _ := strconv.ParseInt(f[4], 10, 64)
		deadStatus, _ := strconv.Atoi(strings.TrimSpace(f[6]))
		rows = append(rows, paneRow{
			session:    f[0],
			paneID:     f[1],
			cwd:        f[2],
			pid:        pid,
			created:    time.Unix(createdUnix, 0),
			dead:       f[5] == "1",
			deadStatus: deadStatus,
			title:      f[7],
			// muster #236: an option that was never set reads back as
			// the empty string, same as any other unset tmux format
			// variable — never "1" — so an unmanaged session's pane always
			// parses to false here.
			managed:  f[8] == "1",
			launchRC: launchRC,
		})
	}
	return rows, nil
}

// splitCaptures divides one concatenated capture stream into per-pane text
// using the nonce marker emitted before each capture.
func splitCaptures(out, mark string) map[string]paneCapture {
	res := map[string]paneCapture{}
	parts := strings.Split(out, mark)
	for _, p := range parts[1:] { // parts[0] is anything before the first marker
		nl := strings.Index(p, "\n")
		if nl < 0 {
			continue
		}
		// The marker line is "<index> <pane height>" (muster#169). A
		// height that does not parse leaves 0, which reads every row as it
		// always was rather than guessing a boundary.
		header := strings.Fields(p[:nl])
		if len(header) == 0 {
			continue
		}
		c := paneCapture{text: p[nl+1:]}
		if len(header) > 1 {
			if h, err := strconv.Atoi(header[1]); err == nil && h > 0 {
				c.height = h
			}
		}
		res[header[0]] = c
	}
	return res
}

// paneCapture is one pane's classify capture and the pane's height when it was
// taken. The height is what tells the visible pane apart from the `-S -N`
// history margin above it (muster#169); 0 means unknown.
type paneCapture struct {
	text   string
	height int
}

// screen parses the capture with its visible-pane boundary.
func (c paneCapture) screen() screen { return newScreenVisible(c.text, c.height) }

// paneHeightFormat is appended to the marker a capture is introduced by, so
// the height arrives in the same invocation as the rows it describes.
const paneHeightFormat = "#{pane_height}"

// List returns every session in one call (§3, and driver.Driver.List's
// contract). The returned Collection always carries exactly one
// SourceStatus — this machine's own — because even a single machine
// answering for itself must say who answered (§9, api-http.md §3.2).
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		// §5.7: a failed read is never an empty result. Report the source
		// as unreachable and let the envelope carry the failure.
		//
		// This is now genuinely "no response": enumerate only returns an
		// error when the LISTING call itself failed (see enumerate's own
		// comment — a capture-side failure is folded into the per-session
		// "unknown" path below instead). Unreachable is the right word
		// exactly because nothing about this machine answered at all.
		// A multiplexer that answered "there is no server" did answer, and
		// enumerate returns that as zero rows rather than as an error
		// (#157), so it never reaches this branch.
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     fleet.SourceUnreachable,
			Error:      err.Error(),
			ObservedAt: d.now(),
			// The multiplexer not answering says nothing about whether the
			// ACCOUNT is refusing work — that memory is this driver's own
			// (quotaBlock, below) and does not depend on reaching tmux at
			// all (#10). Reachability and willingness are different
			// questions; answer both, independently, even when one of them
			// just failed.
			Quota: quotaOnly(d.quotaBlock()),
		}
		return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{src})
	}

	// muster #235: reap what this SAME enumeration already found dead,
	// using the screen it already captured — never a second listing or a
	// second capture-pane call, which would cost every ordinary List an
	// extra invocation (driver.Driver.List's constant-spawn contract) to
	// cover an outcome that is rare by construction. A session whose own
	// process already exited — remain-on-exit is what leaves its pane
	// standing at all, see Create's own comment on the option — must never
	// appear in what this call returns; DrainExits (called by the service
	// right after this List) is how its captured exit reaches the history
	// record instead.
	rows = d.reapDeadRows(ctx, rows, captures)

	// A pane can have vanished between the listing and the capture (see
	// enumerate's own comment and #29): that row survives — it is still a
	// real session this machine reported — but its screen was not read, and
	// captures simply has no entry for it. Count that here, once, rather
	// than at each of the sites below that read captures: this is the one
	// place that gets to decide what a miss says about the SOURCE, as
	// opposed to what it says about the one session that missed.
	missed := 0
	for _, r := range rows {
		if _, ok := captures[r.paneID]; !ok {
			missed++
		}
	}

	// Loaded once and shared by noteSessionSet and identityDrift both
	// (muster #96/#97), rather than reading the store twice per
	// listing. Nil when unconfigured — every reader downstream already
	// treats a nil/empty map as "nothing asserted", the same honest default
	// every other durable record in this driver uses.
	var priorRecords map[string]sessionRecord
	if d.store != nil {
		priorRecords = d.loadRecords()
	}
	drift := identityDrift(rows, priorRecords)
	driftBySession := make(map[string]nameDrift, len(drift))
	for _, nd := range drift {
		driftBySession[nd.live.session] = nd
	}
	// muster #102: a second, independent index over the same prior
	// records, for identityAssertionFor below. Not threaded through
	// identityDrift itself — that function's output also drives
	// reassertNames, and TestIdentityReassertStopsOnceContested /
	// TestReassertRefusesWhenTheNameIsTaken cover that path unchanged; this
	// keeps it that way rather than reshaping it to serve a second reader.
	assertedByRun := indexByPaneCreated(priorRecords)

	d.noteSessionSet(rows, priorRecords)

	sessions := make([]fleet.Session, 0, len(rows))

	// Which sessions still need their conversation record located. Collected
	// here and resolved below, after the lock is dropped: that lookup reads a
	// filesystem, and holding the lock that guards every session's observed
	// state across a directory read would make one slow disk stall the whole
	// listing.
	type pendingConversation struct {
		index   int
		key     conversationKey
		cwd     string
		name    string
		started time.Time
		pid     int
	}
	var pending []pendingConversation

	now := d.now()
	// Read once, outside the loop, and stamp every session in this response
	// with the same value: they are all being answered as of this one
	// instant, and a machine-wide fact read once per session risks reading
	// two different generations into one snapshot if the file changes
	// mid-loop (#12).
	gen := d.credentialGeneration()
	d.mu.Lock()
	for _, r := range rows {
		c, captured := captures[r.paneID]
		young := now.Sub(r.created) < startingWindow
		raw, digest := classifyCaptureRemembering(c, captured, !r.dead, young, d.memoryLocked(r.session), now)
		st, carried := d.stampSinceLocked(r.session, raw, now, captured)
		st.CredentialGeneration = gen
		// Published so a caller can quote it back on a raw key (keys.go). It
		// is already computed for the classifier's own use; empty when the
		// capture failed, which correctly leaves that session unkeyable rather
		// than keyable against a screen nobody read.
		st.ScreenDigest = digest
		// muster #97: this read agreed with a rename that did not
		// hold. Say so in the read itself, not only in the repair
		// attempted below (after the lock) — a caller reading THIS
		// response must not see it agree silently the way #97's own
		// measurement found it doing.
		if nd, ok := driftBySession[r.session]; ok {
			st.Evidence += "; " + driftSentence(nd.want, r.session)
		}
		d.recordObservationLocked(r.session, observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			digestSince:   d.digestSinceLocked(r.session, digest, now),
			sinceRestored: carried,
		}, captured, *st.Since)
		started := r.created
		s := fleet.Session{
			SessionRef: fleet.SessionRef{Machine: d.machine, ID: r.session, Name: r.session},
			StartedAt:  &started,
			Runtime:    d.runtime,
			Cwd:        fleet.AbsolutePath(r.cwd),
			Attach:     d.attachHint(r.session),
			State:      st,
		}
		// muster #102: the same fact the evidence sentence above
		// carries, machine-readable. Populated HERE — at response-build
		// time, from what THIS read observed — not from reassertNames'
		// repair below, which runs after this response is built and lands
		// on the caller's NEXT poll, not this one.
		s.IdentityAssertion = identityAssertionFor(r, priorRecords, assertedByRun)
		// muster #165: the marker this run's create applied, from the
		// same prior records and matched the same way — so a rename, ours or
		// a second actor's, leaves it on the session it describes.
		s.Marker = markerFor(r, priorRecords, assertedByRun)
		// #185: which delivery lane this session's input takes, when an
		// external module is enabled here. Nil for a session with no lane
		// record — absent is "not stated", never "terminal".
		s.Delivery = d.mods.laneView(r.session)
		// #84/#85/#86: this session's own create record, if one is still on
		// file — see createrecord.go. Absent means either nothing was
		// requested that this record would carry, or the record already
		// expired; pinOutcomeFor/promptDeliveryFor/runtimeSurfaceFor tell
		// those apart from their own carried/requested flags, never the
		// presence of the record itself.
		if cr, ok := d.createRecordForLocked(r.session, r.cwd); ok {
			s.Pins = pinOutcomeFor(cr)
			s.PromptDelivery = promptDeliveryFor(cr)
			// #85: the runtime's own footer, already classified into st a
			// few lines up, is the corroboration a dictated identifier
			// needs before RuntimeSurface may claim Known: true (§5.7 —
			// publishing an uncorroborated identifier as a fact is #84's
			// defect in a second field). Latched, never unset: identity,
			// not liveness — a channel that later reads Failed keeps its
			// address and reports the failure through state.controlChannel,
			// which is the field for it.
			if st.ControlChannel != nil && st.ControlChannel.State == fleet.ControlChannelActive && !cr.SurfaceSeen {
				d.noteSurfaceSeenLocked(r.session)
				cr.SurfaceSeen = true
			}
			s.RuntimeSurface = runtimeSurfaceFor(cr, r.session)
		}
		if !matchesFilter(s, filter) {
			continue
		}
		if d.conversations != nil {
			// Keyed on the pane rather than the session name, because a
			// rename changes the name and the title already written into the
			// record does not — see conversationKey.
			pending = append(pending, pendingConversation{
				index:   len(sessions),
				key:     conversationKey{pane: r.paneID, created: r.created},
				cwd:     r.cwd,
				name:    r.session,
				started: r.created,
				pid:     r.pid,
			})
		}
		sessions = append(sessions, s)
	}
	obs := make(map[string]observation, len(d.observed))
	for k, v := range d.observed {
		obs[k] = v
	}
	d.mu.Unlock()
	d.noteStatuses(obs)

	// muster #97: put back every name this driver asserted and the
	// runtime no longer carries — whether the rename never reached the
	// runtime, or reached it and a second actor on the machine later undid
	// it; either way the record, not the last read, is what this driver
	// trusts. After the lock, like the conversation lookups below: each
	// repair is a real multiplexer call, and nothing here may run under the
	// lock that guards every session's observed state.
	d.reassertNames(ctx, rows, drift)

	// Locate each session's record in the runtime's own store. This is the
	// only source on this path that is not the runtime describing itself
	// (conversation.go says why that matters), and it is also the only one
	// that can answer "I looked and could not tell" — which is a different
	// answer from the absent field a driver with no store leaves behind.
	for _, p := range pending {
		conv := d.conversations.lookup(p.key, p.cwd, p.name, p.started, processGeneration{pid: p.pid},
			d.liveConversationSource(ctx, p.pid, p.cwd))
		sessions[p.index].Conversation = conv
		// #72: a session whose CREATE asked to resume a conversation gets
		// that intent compared against what actually resolved, so a resume
		// silently downgraded to a fresh conversation is reported rather
		// than looking like an ordinary healthy start.
		if requested, ok := d.resumeIntentFor(p.name, p.cwd); ok {
			sessions[p.index].ResumeOutcome = resumeOutcomeFor(requested, conv)
		}
		// #224: the mirror check for a create that asked to START a
		// conversation under a caller-chosen id rather than continue one.
		// Unlike ResumeOutcome this REPLACES Conversation rather than adding
		// a sibling field — conversationIntentOutcome's own doc says why: the
		// 201 already told the caller its captured id, and a plain "nobody
		// looked yet" here would be a caller-visible regression from what
		// create already reported, not a new, more honest answer.
		if requested, ok := d.conversationIntentFor(p.name, p.cwd); ok {
			sessions[p.index].Conversation = conversationIntentOutcome(requested, conv)
		}
	}

	// #111: publish `turns` for every session carrying a live delivery
	// mark — independent of the quota/lastTurn gate just below, because
	// "did the agent run at all" is exactly the answer a QUIET session
	// needs most, unlike LastTurn/Quota which only ever upgrade something
	// the screen already flagged. Gated on a delivery mark existing, not on
	// anything the screen flagged: only a session this driver has actually
	// delivered into ever opens its record for this, so a fleet with
	// nothing dispatched through Send pays nothing extra here.
	for i := range sessions {
		sessions[i].State.Turns = d.turnsFor(sessions[i].ID, string(sessions[i].Cwd), sessions[i].Conversation)
	}

	// muster #269: where the screen reader found no label, the runtime's own
	// record and the launch decide between active, off and "not read". Done here
	// because it needs the resolved conversation, which the row loop above does
	// not have; the footer's answer, when there is one, passes through untouched.
	rowByName := make(map[string]paneRow, len(rows))
	for _, r := range rows {
		rowByName[r.session] = r
	}
	for i := range sessions {
		if r, ok := rowByName[sessions[i].ID]; ok && !r.dead {
			sessions[i].State.ControlChannel = d.resolveControlChannel(sessions[i].State.ControlChannel, sessions[i].Conversation, r)
			d.latchSurfaceFromChannel(&sessions[i], r)
		}
	}

	// Ask the runtime's own record about whatever the screen already
	// flagged this cycle (#56) — a usage-limit notice, or a last turn the
	// screen read as failed. Only sessions the screen already flagged pay
	// for this: a quiet session's record is never opened, so a healthy
	// fleet costs nothing extra here. recordUnavailable (the zero value of
	// quotaVerdict when nothing resolves) means every downstream consumer
	// keeps its existing screen-derived fallback unchanged.
	var quotaRecord apiErrorFact
	var quotaVerdict recordVerdict
	for i := range sessions {
		switch {
		case sessions[i].State.Status == fleet.StatusQuotaBlocked:
			if quotaVerdict == recordUnavailable {
				if fact, verdict := d.recordFactFor(sessions[i]); verdict != recordUnavailable {
					quotaRecord, quotaVerdict = fact, verdict
				}
			}
		case sessions[i].State.LastTurn != nil:
			switch fact, verdict := d.recordFactFor(sessions[i]); verdict {
			case recordAPIError:
				sessions[i].State.LastTurn = &fleet.TurnEnd{
					Outcome:   "failed",
					Reason:    fact.reasonSentence(),
					Retryable: fact.retryable(),
				}
			case recordCleanTurn:
				// The durable record says the last turn actually
				// succeeded — the screen's "api error" match was history
				// a window scan cannot tell from the present. #56's
				// argument for Quota, arriving at LastTurn instead.
				sessions[i].State.LastTurn = nil
			}
			// recordUnavailable: keep classify.go's screen-derived
			// TurnEnd exactly as built — still the legitimate fallback
			// when no record store is configured or this session's
			// record cannot be matched.
		}
		// The control channel is orthogonal to what the session is DOING
		// (controlchannel.go's own comment says so, and this is that rule
		// again): a session can be StatusWorking with LastTurn nil and
		// still have a failed channel, so this is its own check rather
		// than another case of the switch above, which a session matching
		// both would only ever enter once.
		if ch := sessions[i].State.ControlChannel; ch != nil && ch.State == fleet.ControlChannelFailed && ch.Reason == "" {
			// Only a session already flagged Failed pays for this, and one
			// resolved from the record (#270) already carries its Reason (#69, the same "quiet session's record is never
			// opened" discipline the switch above already holds). Reason
			// stays empty — never a guess — whenever the record cannot
			// explain why: no store, no matched conversation, or no
			// matching entry.
			if fact, ok := d.controlReasonFor(sessions[i]); ok {
				channel := *ch
				channel.Reason = fact.reasonText()
				sessions[i].State.ControlChannel = &channel
			}
		}
	}

	// A usage limit belongs to the ACCOUNT, not to whichever pane happened to
	// print the notice, and it outlives that notice by days. Observe it once
	// per read: any session showing it sets the block, any session actually
	// working clears it.
	var sawLimit, sawWorking bool
	var hint string
	for i := range sessions {
		switch sessions[i].State.Status {
		case fleet.StatusQuotaBlocked:
			sawLimit = true
			if q := sessions[i].State.Quota; q != nil && q.ResetHint != "" {
				hint = q.ResetHint
			}
		case fleet.StatusWorking:
			sawWorking = true
		}
	}
	d.noteQuotaBlock(sawLimit, hint, sawWorking, quotaRecord, quotaVerdict, now)

	// Apply it. A session that reads idle on a machine whose account is
	// refusing work is not available, and idle is the status that means send
	// it work — the whole failure this exists to prevent.
	//
	// Two statuses are rewritten, and the second was left out at first.
	//
	// idle, because idle is the status that means send it work.
	//
	// unknown, because unknown is not a competing truth — it is this driver
	// saying it could not determine one (§5.7), and an account fact IS more
	// specific than that. Leaving it out had a visible cost: four sessions on
	// a blocked machine flapped unknown → quota_blocked → unknown across
	// consecutive reads, because their panes redraw a counter, so the digest
	// changed and the ambiguity that resolves to idle never settled. Eight
	// spurious state events per cycle on a fleet where nothing was happening.
	//
	// Nothing else is rewritten. working, waiting_input and unsent text each
	// carry something observed just now, and a remembered fact must not
	// overwrite an observation.
	//
	// The same corroboration works in reverse, and must. A limit notice sits
	// on the screen long after the limit lifts — nobody types into a session
	// that refused them, so nothing overwrites it — and read literally it says
	// "blocked" forever. Measured on a working machine: two sessions reporting
	// quota_blocked from notices that had already expired, while two others on
	// that same account were working.
	//
	// A session working NOW is proof the account is not refusing work, and it
	// outranks a screen that has not changed since it was refused. So the
	// notice becomes what it is — history — and the session reports what it
	// otherwise is: settled, with nothing running and no question pending.
	// This is F53's divider again, with the evidence in another pane instead
	// of a lower line.
	//
	// recordCleanTurn joins sawWorking as an equally sufficient reason (#56):
	// a blocked session's OWN durable record showing its most recent turn
	// succeeded is the same kind of positive proof, from a source that
	// outlives the notice scrolling off the one pane that showed it.
	if sawWorking || quotaVerdict == recordCleanTurn {
		for i := range sessions {
			if sessions[i].State.Status != fleet.StatusQuotaBlocked {
				continue
			}
			st := sessions[i].State
			st.Status = fleet.StatusIdle
			st.Quota = nil
			if quotaVerdict == recordCleanTurn {
				st.Evidence = "a limit notice is on screen, but the runtime's own record shows " +
					"a later turn on this account already succeeded, so the notice is history"
			} else {
				st.Evidence = "a limit notice is on screen, but another session on this account is working now, so the notice is history"
			}
			sessions[i].State = st
		}
	}

	// q is read once and reused below for the source's own Quota field
	// (#10) — the same remembered fact, at both grains, from a single
	// lock acquisition rather than two.
	q, sinceObserved := d.quotaBlock()
	if q != nil {
		for i := range sessions {
			sessions[i].State = quotaBlockedState(sessions[i].State, q, sinceObserved)
		}
	}

	// Every block carries a real since. The per-session path builds its
	// QuotaBlock in classify, which has no clock, so it left the zero time —
	// which serialises as year 1 and is worse than absent: a caller computing
	// "blocked for how long" gets two millennia.
	//
	// This is exactly the session whose OWN screen showed the notice
	// directly (quotaBlockedState's rewrite only touches idle/unknown/
	// starting, by design — a session already reporting quota_blocked keeps
	// classify.go's own evidence phrase). q's own Since/sinceObserved — the
	// account-level fact noteQuotaBlock just finished maintaining, whether
	// from THIS cycle's record or restored from a restart — is the
	// authoritative answer (#56) and applies here first; the status's own
	// since (this driver's first observed TRANSITION into the status, still
	// a sighting rather than the refusal) and the read time are fallbacks
	// for when there is no account-level block to consult at all.
	for i := range sessions {
		st := sessions[i].State.Quota
		if st == nil || !st.Since.IsZero() {
			continue
		}
		blocked := *st
		recordConfirmed := false
		switch {
		case q != nil && !q.Since.IsZero():
			blocked.Since = q.Since
			recordConfirmed = sinceObserved
			if q.ResetHint != "" && blocked.ResetHint == "" {
				blocked.ResetHint = q.ResetHint
			}
		case sessions[i].State.Since != nil && !sessions[i].State.Since.IsZero():
			blocked.Since = *sessions[i].State.Since
		default:
			blocked.Since = now
		}
		sessions[i].State.Quota = &blocked
		if recordConfirmed {
			sessions[i].State.Evidence += "; since is the runtime's own record of the refusal"
		} else {
			sessions[i].State.Evidence += "; since is when this driver first observed the notice, " +
				"not when the refusal happened — the runtime's own record could not confirm it"
		}
	}

	count := len(sessions)
	src := fleet.SourceStatus{
		Machine:    d.machine,
		Status:     fleet.SourceOK,
		Count:      &count,
		ObservedAt: now,
		// A scheduler asking "where can work run right now" reads this
		// field, not Items() — the account fact must be visible here even
		// against a filter that empties the item list entirely (#10).
		Quota: q,
	}
	if missed > 0 {
		// This machine answered — every session in count is real, including
		// the ones that missed a capture — so unreachable would be a lie.
		// But it did not answer IN FULL, and §5.7's rule that absence and
		// failure are different answers applies to the source's own status
		// exactly as it does to a session's: reporting SourceOK here would
		// have this read call itself complete while N of its sessions are
		// carrying "unknown" for a reason the caller cannot see without
		// this Error string. NewCollection reads this Status and turns
		// Complete() false on its own — nothing below has to remember to.
		src.Status = fleet.SourceDegraded
		src.Error = fmt.Sprintf(
			"%d of %d sessions' screens were not captured this read "+
				"(a pane vanished between listing and capture, or the capture "+
				"invocation otherwise failed for it); each reports its own "+
				"state as unknown rather than a guess",
			missed, len(rows))
	}
	return fleet.NewCollection(sessions, []fleet.SourceStatus{src})
}

// maxNameReasserts bounds how many times reassertNames will put an asserted
// name back after finding the runtime disagreeing with it (muster
// #97). A repair already proven not to hold twice is not attempted a third
// time — discardProvenFutile's rule (this file, the composer-clear case)
// applied to identity: an unbounded loop against a second actor on the
// machine that keeps reverting a name is a rename war, not a fix.
const maxNameReasserts = 2

// Counter names for identity repair (muster #96/#97) — see counterSet's
// own doc comment on why these are a registry entry rather than a new field.
const (
	// counterIdentityReasserted counts every time List successfully put an
	// asserted name back after finding the runtime disagreeing with it.
	counterIdentityReasserted = "identity.reasserted"
	// counterIdentityReassertFailed counts a reassert attempt whose own
	// rename-session call failed.
	counterIdentityReassertFailed = "identity.reassert_failed"
	// counterIdentityContested counts a drift entry List declined to
	// repair: the wanted name is itself live under a different session, or
	// maxNameReasserts is already spent.
	counterIdentityContested = "identity.contested"
)

// reassertNames puts back every name in drift — muster #97: a rename
// this driver recorded and the runtime no longer carries, whether because
// it never reached the runtime or reached it and was later undone by a
// second actor on the machine. Called from List, after its own
// d.mu.Unlock(): each repair is a real multiplexer call, and none of them
// may run under the lock that guards every session's observed state.
func (d *Driver) reassertNames(ctx context.Context, rows []paneRow, drift []nameDrift) {
	if len(drift) == 0 {
		return
	}
	liveNames := make(map[string]bool, len(rows))
	for _, r := range rows {
		liveNames[r.session] = true
	}
	for _, nd := range drift {
		if liveNames[nd.want] {
			// Another live session already carries the name this driver
			// wants to restore. Refuse rather than let the multiplexer
			// decide — the same rule Rename itself applies at request
			// time (§ above) — and stop trying: a name genuinely taken by
			// someone else does not become free by retrying.
			d.recordContested(nd.live.session, nd.rec)
			d.counters.incr(counterIdentityContested)
			continue
		}
		if nd.rec.Reasserts >= maxNameReasserts {
			d.counters.incr(counterIdentityContested)
			continue
		}
		if _, err := d.run(ctx, d.bin, "rename-session", "-t", "="+nd.live.session, nd.want); err != nil {
			d.recordReassertAttempt(nd.live.session, nd.rec, false)
			d.counters.incr(counterIdentityReassertFailed)
			continue
		}
		d.mu.Lock()
		if o, ok := d.observed[nd.live.session]; ok {
			d.observed[nd.want] = o
			delete(d.observed, nd.live.session)
		}
		d.mu.Unlock()
		d.recordReassertAttempt(nd.live.session, nd.rec, true)
		d.counters.incr(counterIdentityReasserted)
	}
}

func matchesFilter(s fleet.Session, f driver.ListFilter) bool {
	if f.Status != "" && s.State.Status != f.Status {
		return false
	}
	if f.Agent != "" && s.Agent != f.Agent {
		return false
	}
	if f.CwdPrefix != "" && !strings.HasPrefix(string(s.Cwd), f.CwdPrefix) {
		return false
	}
	return true
}

// State reads one session. It is implemented over the same batched
// enumeration rather than a targeted query, because at these costs a whole
// fleet read is cheaper than the two subprocess spawns a targeted read
// would need, and it keeps exactly one code path deciding what a status
// means.
func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.SessionState{}, err
	}
	for _, r := range rows {
		if r.session != ref.ID {
			continue
		}
		now := d.now()
		c, captured := captures[r.paneID]
		d.mu.Lock()
		raw, digest := classifyCaptureRemembering(c, captured, !r.dead,
			now.Sub(r.created) < startingWindow, d.memoryLocked(r.session), now)
		st, carried := d.stampSinceLocked(r.session, raw, now, captured)
		st.CredentialGeneration = d.credentialGeneration() // #12, same as List's per-session stamp
		st.ScreenDigest = digest                           // see List's stamp of the same field
		if st.WaitingOn == fleet.WaitingUnsentInput {      // #240, same as List
			st.StrandedDelivery = d.ownDeliveryLocked(r.session, r.cwd, c, captured)
		}
		d.recordObservationLocked(r.session, observation{
			created: r.created, cwd: r.cwd, at: now,
			status: st.Status, statusSince: *st.Since, digest: digest,
			digestSince:   d.digestSinceLocked(r.session, digest, now),
			sinceRestored: carried,
		}, captured, *st.Since)
		d.mu.Unlock()
		// Same record upgrade List applies to LastTurn (#56) — State has no
		// pre-resolved Conversation to reuse (it returns a bare
		// SessionState, not a Session), so this does its own lookup; that
		// lookup memoises successes in conversationStore, so a session List
		// already resolved this cycle costs a map read here, not a rescan.
		st = d.upgradeLastTurnFromRecord(ctx, st, r.cwd, r.session, r.created, r.paneID, r.pid)
		// Same record upgrade List applies to ControlChannel.Reason (#69),
		// same reason State does its own lookup rather than reusing a
		// resolved Conversation.
		st = d.upgradeControlChannelFromRecord(ctx, st, r.cwd, r.session, r.created, r.paneID, r.pid)
		// muster #269: where the screen found no label, the record and the launch
		// decide — see resolveControlChannel. State does its own conversation
		// lookup for the same reason it does the two upgrades around this line.
		if !r.dead && d.conversations != nil {
			conv := d.conversations.lookup(conversationKey{pane: r.paneID, created: r.created}, r.cwd, r.session, r.created,
				processGeneration{pid: r.pid}, d.liveConversationSource(ctx, r.pid, r.cwd))
			st.ControlChannel = d.resolveControlChannel(st.ControlChannel, conv, r)
		}
		// #111: same split as the two upgrades just above — List resolves
		// `turns` from its own pre-resolved Conversation, State does its own
		// lookup.
		st = d.upgradeTurnsFromRecord(ctx, st, r.cwd, r.session, r.created, r.paneID, r.pid)
		// Same rewrite List applies, generalised to a one-session read (#10)
		// — see quotaBlockedState's own comment for why a session's own
		// state must not be reported as an unqualified "starting"/"idle"/
		// "unknown" while this machine's account is known to be refusing
		// work. This is the read Create's own HTTP handler makes to build
		// the state it hands back in a 201 — without this, a create
		// reported nothing and the fact was swallowed until a later poll.
		q, sinceObserved := d.quotaBlock()
		return quotaBlockedState(st, q, sinceObserved), nil
	}
	// §5.7 applied to a singular read, and then applied a second time to its
	// own answer.
	//
	// "I looked and it is not there" is a real answer, not a failure to look
	// — that much was always right. What was wrong is that it was the ONLY
	// answer: every unfound id returned `dead`, including ids this machine
	// has never had.
	//
	// `dead` is a claim about history — it existed, and it ended. For a
	// mistyped id there is no such history, so the claim is manufactured, and
	// a caller gets told its session died when the truth is that no such
	// session was ever here. Those deserve opposite reactions.
	//
	// The driver's own memory settles it, and that memory already exists for
	// §8's `since` and §12's reconciliation.
	d.mu.Lock()
	prior, seen := d.observed[ref.ID]
	d.mu.Unlock()
	if !seen {
		return fleet.SessionState{}, d.noSuchSession(ctx, rows, ref.ID)
	}
	evidence := "session was present in the multiplexer and is no longer"
	if prior.cwd != "" {
		// Name what is gone. A caller reconciling its own records needs to
		// know WHICH session ended, and an id alone is recyclable (§5.4).
		evidence += "; last seen in " + prior.cwd
	}
	return fleet.InferredState(fleet.StatusDead, evidence, nil), nil
}

// Send delivers input to a session (§3), and refuses when delivery would
// corrupt it (§2.4).
//
// The refusal case is the reason this operation is not a boolean. If a
// human has typed into the composer and not submitted, pasting more text
// concatenates the two into one message that neither party wrote. §2.4
// names this scenario exactly ("injecting text into a prompt that already
// holds unsent input the human typed") and puts the protection in the
// contract rather than in each caller's memory of a past incident. This is
// that protection, implemented.
//
// Text is delivered via the multiplexer's paste buffer rather than as
// simulated keystrokes. Keystroke simulation would require escaping the
// caller's text against the multiplexer's key-name vocabulary, where a
// message containing something like "C-c" is a live hazard; the paste
// buffer takes bytes and interprets none of them.
func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	receipt, err := d.send(ctx, req, ref, text, opts)
	// #184: every Send is counted under route.* exactly once, whichever return
	// below produced it.
	d.observeRoute(opts.Route, receipt, err)
	return receipt, err
}

// send is Send's body; Send only adds the route counters around it.
func (d *Driver) send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	// Terminal path v2 / D4: every composer-touching operation on this
	// session id is serialised through one lock, acquired before anything
	// else runs and released on every return from here on
	// (terminalpath2_lock.go). This runs even ahead of #53's own guard
	// below, which is deliberately about the BYTES alone and untouched —
	// the lock is about WHICH CALL gets to look at and change this
	// session's composer next, a different axis, and there is no cost to
	// acquiring it before a call that is about to refuse anyway.
	unlockComposer, lockOK := d.lockComposerOpsCtx(ctx, ref.ID)
	if !lockOK {
		return d.observeEarly(delivery.RefusedBusy, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  composerBusyReason(""),
		}), nil
	}
	defer unlockComposer()

	// Review fix: sanitise ONCE, here, before anything downstream forms an
	// opinion about these bytes — including the #53 guard immediately below.
	// Every comparison and every delivery from this point on uses THIS same
	// sanitised string: the guard, the inbox path, paneLabelled's label, the
	// paste, confirmLandedV2/confirmSubmittedV2's comparisons, and the
	// stranded record. Sanitising a second time later (pasteBracketed still
	// does, defensively) is a no-op once this has already run.
	//
	// # Why this closes the #53 bypass
	//
	// The guard used to run on the CALLER'S text while pasteBracketed
	// sanitised a COPY of it moments later — two different strings, one
	// decision made against the wrong one. `"\v!id"` is not read as
	// runtime syntax by the guard's own trim (it does not include \v), so it
	// passed; the sanitiser then dropped the \v anyway and pasted `"!id"`,
	// which IS runtime syntax — refused nowhere. Sanitising first means the
	// guard sees exactly what will be pasted, so a control byte can no
	// longer manufacture a leading "!" that only appears after the check
	// that was supposed to catch it.
	//
	// # Why this closes the "sanitised vs unsanitised compare" gap
	//
	// composerRegionMatch, the legacy needle and transcriptTurnMatches used
	// to compare the PASTED (sanitised) text against the CALLER'S (raw)
	// text, so any payload containing a control byte could never confirm —
	// the composer and the transcript both hold the sanitised form, and
	// normalizeForMatch strips only whitespace, not control bytes. Using one
	// sanitised string everywhere removes the mismatch structurally instead
	// of teaching each comparison to sanitise its own side.
	text = sanitizeForBracketedPaste(text)

	// #53: fail closed on text this runtime reads as its own syntax rather
	// than a message, BEFORE anything else. This is a decision about the
	// BYTES, not about the moment — it must not depend on which session
	// exists, what its screen shows, or whether the substrate is reachable
	// at all, so it runs ahead of every one of those and the substrate is
	// never touched for text that was always going to be refused. See
	// inputguard.go for the pattern list and why it belongs to this driver.
	if reason, refused := refuseAsRuntimeSyntax(text, opts.HumanRelay); refused {
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}), nil
	}
	// #272: the remote-control commands are an exposure change, so with the
	// gate on they need the remote-control (or human-relay) grant on top of
	// send. Same bytes, same place, same reason as the guard above.
	if reason, refused := refuseRemoteControlCommand(text, d.gateRemoteControlInput, opts.HumanRelay, opts.RemoteControl); refused {
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}), nil
	}

	// muster #112: ResumeIfStranded asks to finish the delivery already
	// sitting in the composer; ReplaceIfStranded asks to throw it away and
	// deliver this call's text instead. Both at once is a contradiction, not
	// an ambiguity to resolve by picking one silently — refused before
	// anything else runs, the same way #53's guard above decides on the
	// bytes alone before looking at session state.
	if opts.ResumeIfStranded && opts.ReplaceIfStranded {
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "resumeIfStranded and replaceIfStranded were both set — the first asks " +
				"to finish the delivery already in the composer, the second asks to " +
				"discard it and deliver this text instead; set at most one",
		}), nil
	}

	// #184: which path. The service has already decided everything that depends
	// on WHO is sending (a human relay's auto arrives here as terminal); what is
	// decided here depends on the SESSION — whether its inbox can take this
	// message, and what to do when it cannot.
	route := opts.Route
	if route == "" {
		route = fleet.RouteAuto
	}
	if !route.Valid() && !d.deliveryModuleEnabled(string(route)) {
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  fmt.Sprintf("route %q is not one this driver accepts (auto, terminal or inbox)", string(route)),
		}), nil
	}
	if d.deliveryModuleEnabled(string(route)) && (!opts.Submit || opts.ResumeIfStranded || opts.ReplaceIfStranded) {
		// #185: the same shape rule the service applies, stated again because a
		// driver can be called without it. A module has no composer, so a
		// forced module route is never quietly turned into a composer
		// operation.
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: fmt.Sprintf("route %q delivers a message as a turn: it cannot be combined with "+
				"submit:false, resumeIfStranded or replaceIfStranded, which name a composer a delivery "+
				"module does not have. Nothing was written", string(route)),
		}.WithModule(string(route))), nil
	}
	if route == fleet.RouteInbox && (!opts.Submit || opts.ResumeIfStranded || opts.ReplaceIfStranded) {
		// The same shape rule the service applies, stated again because a driver
		// can be called without it. An explicit inbox request is never quietly
		// turned into a composer operation.
		return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "route \"inbox\" delivers a message as a turn: it cannot be combined with " +
				"submit:false, resumeIfStranded or replaceIfStranded, which name a composer the inbox " +
				"does not have. Nothing was written",
		}.WithRoute(fleet.RouteInbox)), nil
	}

	// #257: a live delivery lane is the session's only input path. For a
	// caller's /input (LiveLaneOnly) on a session whose lane is usable right
	// now, a shape that would reach the terminal instead is refused with
	// nothing written. A named inbox keeps its own rules; a forced module was
	// judged above. pinned remembers the live module so the module step below
	// refuses, instead of falling back to the terminal, if the lane is lost
	// between here and the write.
	pinned := ""
	if opts.LiveLaneOnly && (route == fleet.RouteAuto || route == fleet.RouteTerminal) {
		if module, _, _, why := d.mods.liveLane(ref.ID, ""); why == "" {
			var asked []string
			if route == fleet.RouteTerminal {
				asked = append(asked, "terminal")
			}
			if !opts.Submit {
				asked = append(asked, "submit_false")
			}
			if opts.ResumeIfStranded {
				asked = append(asked, "resume")
			}
			if opts.ReplaceIfStranded {
				asked = append(asked, "replace")
			}
			if len(asked) > 0 {
				d.counters.incr(counterRouteRefusedLaneLive)
				for _, f := range asked {
					d.counters.incr(counterRouteRefusedLaneLivePrefix + f)
				}
				return d.observeEarly(delivery.Refused, fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: fmt.Sprintf("this session's delivery lane (module %q) is live, and while it is the lane is the "+
						"session's only input path: route \"terminal\", submit:false, resumeIfStranded and "+
						"replaceIfStranded are not honoured on a live lane. Send again without them and the text "+
						"goes through the lane; to clear text left in the composer use discard with expect. "+
						"Nothing was written", module),
				}), nil
			}
			pinned = module
		}
	}

	// #184: the cross-path ledger, before any path is chosen. An earlier inbox
	// write of this same text that could not be confirmed may already be in the
	// receiver's hands: nothing is written on EITHER path, whatever the flags —
	// a resumeIfStranded retry is a terminal operation and would paste it again.
	if receipt, held := d.answerFromLedger(ctx, ref, deliveryKey(text, opts.From)); held {
		return receipt, nil
	}

	// muster #158: on the terminal path the text carries the sender label
	// as its first line. Everything below — the stranded-delivery record
	// included — sees the labelled text, so a resume must repeat the same `from`
	// as well as the same text. Computed here, after the #53 guard has judged
	// the caller's own text: prefixing first would move a leading slash command
	// off the first line and past the guard.
	labelled := paneLabelled(text, opts.From)

	// muster #119: capability-detected inbox path over a target session's
	// own inbox, tried before anything below touches the pane. inboxEligible
	// excludes every shape (!Submit, ResumeIfStranded, ReplaceIfStranded, a
	// forced terminal route) that names a pane-composer concept the inbox has
	// none of — those calls fall straight through to the terminal path.
	// sendViaInbox manages its own bounded context internally (mirroring
	// ResolveProcessIdentity/VerifyProcessIdentity, which it calls); it does not
	// use the `ctx` this function bounds below, because it must run — and
	// possibly return — before that bound exists.
	if inboxEligible(opts) {
		switch {
		case d.terminalUnconfirmed(ref.ID, labelled):
			// The composer already holds this text as a stranded delivery of
			// ours. The inbox would deliver it once and leave a copy waiting to
			// be submitted — twice — so the inbox is not used. Auto goes on to
			// the terminal, whose own stranded-record rules decide; an explicit
			// inbox request is refused, never quietly sent to the terminal.
			d.counters.incr(counterRouteGuardTerminalUnconfirm)
			if route == fleet.RouteInbox {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "an earlier terminal delivery of this same text is unconfirmed and still in the " +
						"session's composer, so it was not also sent to the inbox. Nothing was written",
				}.WithRoute(fleet.RouteInbox), nil
			}
		default:
			res, err := d.sendViaInbox(ctx, ref, text, opts)
			if err != nil {
				return fleet.DeliveryReceipt{}, err
			}
			if res.Final {
				return res.Receipt, nil
			}
			// Declined with nothing written: a named inbox is refused, never
			// quietly sent to the terminal (#257: auto never reaches here).
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "route \"inbox\" was requested but the session cannot take it: " + res.Why +
					". Nothing was written",
			}.WithRoute(fleet.RouteInbox), nil
		}
	}

	// #185: an optional external delivery module, when this session holds a
	// lane and the send is eligible for it (modulelane_send.go). Nothing has
	// been written when it hands the send back, so the built-in module below
	// carries it — for `auto` only; a forced module route is refused instead.
	if forced, ok := d.moduleRouteChoice(route, opts); ok {
		if receipt, handled := d.sendViaModule(ctx, req, ref, text, labelled, opts, forced, pinned); handled {
			return receipt, nil
		}
	}

	// #180: everything above decides about the REQUEST; how the text reaches
	// the session is the delivery module's (internal/delivery). Callers see
	// the module's receipt verbatim, plus the path it took.
	mod := d.deliveryModule()
	res, err := mod.Deliver(ctx, delivery.Delivery{Req: req, Ref: ref, Text: labelled, Opts: opts})
	if err != nil {
		d.counters.incr(delivery.CounterPrefix + mod.Name() + ".error")
		return fleet.DeliveryReceipt{}, err
	}
	delivery.Observe(d.counters.incr, mod.Name(), res)
	return res.Receipt.WithRoute(fleet.RouteTerminal), nil
}

// deliverViaPane is the built-in tmux delivery module's mechanism (#180):
// terminal path v2 — a bracketed, sanitised paste into the session's
// composer, a composer-region landed check, the wake-key submit, and
// transcript-first confirmation. Send has already taken the session's
// composer lock and settled everything that is a decision about the request
// alone; tr collects what this delivery amounted to, for the module's
// counters.
func (d *Driver) deliverViaPane(ctx context.Context, ref fleet.SessionRef, text string, opts driver.SendOptions, tr *sendTrace) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	var target *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return d.noSuchSessionReceipt(ctx, rows, ref.ID), nil
	}
	if target.dead {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "session process has exited",
		}, nil
	}

	// THE RUNTIME MUST BE ABLE TO RECEIVE BEFORE ANYTHING IS DELIVERED.
	//
	// Measured: delivering to a session that has not finished starting renders
	// the text in the composer and drops the submit, two runs in three, while
	// the receipt still said "queued". Nothing here looked, because Create
	// returns as soon as the process is spawned and Send trusted that.
	//
	// The check is the composer's presence, not a delay — see receptive for
	// why that is evidence about the input path rather than about elapsed
	// time, and for what it does not prove.
	//
	// It refuses rather than waiting out a startup, and that is a deliberate
	// reading of §4.4: a runtime takes far longer to paint than this driver's
	// declared deadline allows, so a Send that blocked until a new session was
	// ready would overrun its own declaration. A refusal is also the honest
	// outcome — §2.4 exists for input that would corrupt a session, and text
	// delivered into a runtime that is not listening strands exactly that way.
	if ready, blocked := d.awaitReceptive(ctx, target.paneID); !ready {
		// muster#215: the runtime's feedback-draft card over a composer this
		// driver cannot read as a whole is the one screen here that is neither a
		// startup nor a dialog, and both wordings below were wrong about it — one
		// blamed startup, the other named a menu. Named first, from a fresh read.
		if reason, isCard := d.feedbackCardRefusalFor(ctx, target.paneID); isCard {
			d.counters.incr(counterFeedbackCardRefusedSend)
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
		}
		if blocked {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is showing a selection menu; delivered text would " +
					"drive the menu rather than be received as input (§2.4)",
			}, nil
		}
		// muster #64: no composer painted is one fact with (at least) two
		// causes, and the old wording asserted one of them as if it were
		// established — "still starting or is not listening" — when the
		// actual observation is only "no composer". A runtime showing a
		// full-screen interface with no composer of its own (a dialog, for
		// one) paints exactly this way too, and reads as "possibly broken"
		// through a message that guessed wrong.
		//
		// `young` is the same discriminator classify.go already uses for the
		// identical ambiguity (starting vs. unknown) — reused here rather
		// than invented, so the two places that hit this shape stay
		// consistent rather than developing their own private judgment calls.
		young := d.now().Sub(target.created) < startingWindow
		if young {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is not able to receive input yet: no composer has been " +
					"painted, and the session is young enough to still be starting. " +
					"Delivering now would render the text and lose the submit. Wait for " +
					"the session to report idle, then send again",
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is not able to receive input yet: no composer has been " +
				"painted, and the session is old enough that this is unlikely to be " +
				"ordinary startup. That could still mean the runtime is slow to start, " +
				"or it could mean the runtime is showing a full-screen interface with " +
				"no composer of its own — a dialog, for one — which this driver cannot " +
				"tell apart from here. Delivering now would render the text and lose " +
				"the submit. If the session does not settle to idle on its own, keys() " +
				"can still reach the screen directly (deliversRawKeys: true)",
		}, nil
	}

	// Re-read the screen AFTER the readiness gate. The enumeration above was
	// taken before the wait, and acting on it here would decide "is somebody
	// typing" from a screen that is now stale by as long as the wait took.
	//
	// Through captureForClassify, which owns the escape-carrying shape this
	// decision depends on. The first version of this re-read dropped the
	// escapes and so read the composer's dim placeholder as text a human had
	// typed — refusing delivery to any idle session showing a hint, and
	// blaming an operator who did not exist.
	screenNow := captures[target.paneID].screen()
	if sc, ok := d.captureForClassify(ctx, target.paneID); ok {
		screenNow = sc
	}

	// A blocking menu is its own refusal, named honestly. Pasting text into a
	// selection prompt does not deliver a message — it drives the menu, which
	// is a different and worse kind of corruption than concatenation.
	if awaitingSelection(screenNow) {
		if reason, isCard := feedbackNoticeRefusal(screenNow); isCard {
			d.counters.incr(counterFeedbackCardRefusedSend)
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session is showing a selection menu; delivered text would " +
				"drive the menu rather than be received as input (§2.4)",
		}, nil
	}

	pending, composerScanResult := composerText(screenNow)
	if composerScanResult == composerClipped {
		// muster#216: a composer cut off by the bottom of the pane reads
		// clipped, where it read absent before and was refused by the readiness
		// gate above with the card named. The card is still the better answer
		// when it is what cut the composer off.
		if reason, isCard := feedbackNoticeRefusal(screenNow); isCard {
			d.counters.incr(counterFeedbackCardRefusedSend)
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
		}
		// muster#134: this driver's own capture ended above the
		// composer's opening fence, so it cannot confirm the composer is
		// empty before delivering. Fail closed, the same direction §2.4
		// already takes for a composer it CAN read and finds busy — the
		// alternative is concatenating onto text this driver never saw.
		d.counters.incr(counterComposerClippedRefusedSend)
		d.countClippedCause(screenNow)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "session's composer " + composerClippedCause(screenNow) + ", " +
				"so it cannot confirm the composer is empty before delivering; " +
				"sending now risks concatenating onto text it cannot see (§2.4). " +
				clippedComposerRemedy,
		}, nil
	}
	// muster#215, #217: with the runtime's feedback-draft card on screen — in
	// any of its states — or its question about turning drafts off, a composer
	// holding a single digit is read as the notice's own shortcut: the text would
	// open the review, ask to send (or, on the confirmation, send) the draft, or
	// dismiss it or turn drafts off, instead of being delivered. The paste is not
	// the hazard for any other text; only a message that is nothing but that digit
	// is.
	if composerScanResult == composerFound && pending == "" && loneDigit(text) && feedbackNoticeOnScreen(screenNow) {
		d.counters.incr(counterFeedbackCardRefusedDigit)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the session is showing one of the runtime's feedback notices (the draft card, " +
				"or its question about turning drafts off), which read a composer holding a single " +
				"digit as their own shortcut (1 opens the review, 2 asks to send the draft and, on " +
				"the confirmation, sends it, 0 dismisses it or turns drafts off): this text would " +
				"drive the notice rather than be received as a message. Say it in more than one " +
				"character. Nothing was written",
		}, nil
	}
	// Review fix (#180 review): resumeIfStranded against a composer
	// that reads EMPTY must not silently fall through to the ordinary
	// fresh-paste path below when this driver holds a matching stranded
	// record — that fresh paste has no memory of the record at all, and
	// delivers a byte-for-byte duplicate of text the runtime may already
	// have accepted (composerText emptying is exactly what a genuine accept
	// looks like). Checked BEFORE the composerFound-and-busy branch below,
	// which only ever runs while the composer still holds something; this is
	// its empty-composer sibling.
	if opts.ResumeIfStranded && composerScanResult == composerFound && pending == "" {
		if record, hasRecord := d.strandedRecordFor(ref.ID, target.cwd); hasRecord && record.Text == text {
			if record.TranscriptPath != "" {
				if result, _ := transcriptTailScan(record.TranscriptPath, record.TranscriptOffset, text); result == transcriptScanMatched {
					d.forgetStranded(ref.ID)
					d.counters.incr(counterResumeConfirmedByTranscriptOnEmptyComposer)
					tr.resumed = true
					return fleet.DeliveryReceipt{
						Outcome: fleet.OutcomeQueued,
						Reason: "resumeIfStranded found the composer already empty, and this " +
							"driver's own transcript record (resolved when the delivery first " +
							"stranded) shows the runtime already accepted this exact text as a " +
							"turn; not re-pasting a duplicate",
					}, nil
				}
			}
			// #180 M1: a delivery whose paste never rendered and for which
			// no Enter was ever pressed cannot have become a turn unless the
			// transcript just checked says so, and it did not. Paste it
			// again, through the ordinary path below — every check a fresh
			// delivery makes (the foreground, the landed check, the last look
			// before Enter) still applies.
			if record.NeverRendered {
				d.counters.incr(counterResumeRepastedNeverRendered)
				tr.resumed = true
			} else {
				// Either no transcript could be resolved at strand time, or it
				// did not show this text accepted within its own window. Refuse
				// rather than silently falling through to a fresh paste of the
				// same text below — that fresh paste IS the duplicate-delivery
				// hazard this check exists to prevent. The record is kept.
				d.counters.incr(counterResumeRefusedEmptyComposerUnconfirmed)
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason: d.withRestartNoteReason(ref.ID, "resumeIfStranded was set for a delivery "+
						"this driver stranded earlier, and the composer no longer holds it, but this "+
						"driver could not confirm from its own transcript record that the runtime "+
						"accepted it either — pasting it again here risks delivering a duplicate. "+
						"The record is kept; drop resumeIfStranded to send fresh text instead if the "+
						"intent really is a new delivery, or read the session first"),
				}, nil
			}
		}
	}

	if composerScanResult == composerFound && pending != "" {
		// The one case where a busy composer is not somebody else's business:
		// this driver put the text there itself, could not confirm it, and
		// said so. Completing that delivery is finishing the caller's original
		// request, not starting a new one.
		//
		// Established from OUR OWN RECORD, never by reading the screen back —
		// a multi-line paste renders as a collapsed summary, so the bytes are
		// not there to compare (F49), and the messages most likely to strand
		// are exactly the long ones.
		resumeRecord, hasResumeRecord := d.strandedRecordFor(ref.ID, target.cwd)
		resumeSameText := opts.ResumeIfStranded && hasResumeRecord && resumeRecord.Text == text
		if resumeSameText {
			// Terminal path v2 / item e: when this driver DID manage to read
			// a definite composer digest at the moment it stranded this
			// delivery (record.ComposerDigest != ""), resumeIfStranded may
			// only resubmit while the composer's CURRENT content still
			// digests to that SAME value — the same corroboration #112's
			// ReplaceIfStranded path already requires (tryReplaceStranded,
			// below). Guards against a human attaching and typing something
			// new into the composer in the gap between the strand and this
			// call, which the wake key a few lines down would otherwise
			// silently submit as if it were this driver's own text.
			//
			// An EMPTY record.ComposerDigest is deliberately NOT treated the
			// same as a mismatch. It means this driver could not read a
			// definite composer state at strand time — most commonly because
			// the very reason the delivery stranded was that the paste had
			// not rendered yet (the noEcho/slow-render shape confirmLanded's
			// own timeout exists for), which is resumeIfStranded's PRIMARY,
			// intended use: the text keeps landing after this driver gave up
			// watching, and a later resume finishes it once it has. Refusing
			// unconditionally on an empty digest would break exactly that
			// case — there is nothing to corroborate against, so this falls
			// back to the protection resume already had before this change:
			// strandedMatches (id+cwd+text) above, plus confirmLandedV2's own
			// fresh, live re-verification against the composer a few lines
			// down. Empty-digest here is a fact about what this driver could
			// observe, not permission to skip corroboration — it is the same
			// distinction composerAbsent vs. composerClipped already draws
			// elsewhere in this package (muster#134): "found nothing"
			// and "could not tell" are different findings, and only the
			// first licenses treating the gap as harmless.
			curDigest := composerTextDigest(pending)
			if resumeRecord.ComposerDigest != "" && !composerDigestMatches(resumeRecord.ComposerDigest, pending) &&
				!composerMatchesText(screenNow, text, false) {
				tr.draftKept = true
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "resumeIfStranded requires the composer's current content to digest " +
						"to the one this driver recorded when it stranded this delivery, and it " +
						"does not; something may have changed the composer since. Use " +
						"replaceIfStranded to discard whatever is there now and deliver this text " +
						"instead, or read the session and discard the composer directly first " +
						"(discard?expect=" + curDigest + ")",
				}, nil
			}

			// #101: this branch only ever runs when the composer is already
			// holding OUR OWN pending delivery (strandedMatches, just above).
			// opts.Submit is not re-checked here: resume is a completion of a
			// submit already requested by the call that stranded this text,
			// not a fresh request that could reasonably ask to land-without-
			// submitting.
			//
			// #109 corrects the rest of this paragraph as it used to read: it
			// claimed "there is nothing left to land, only to submit", treating
			// strandedMatches as proof the paste had already FINISHED landing.
			// It has not — strandedMatches corroborates WHOSE delivery this is,
			// never whether it settled. See the `landed` check a few lines down
			// for what replaced that claim.
			//
			// Wake key before the newline, the same shape as the other two
			// submit sites (#21). This pane is idle BY DEFINITION — the branch
			// only runs when a composer has been sitting on an unsubmitted
			// line — so if a lone newline is ever dropped there, it is dropped
			// here.
			//
			// The measurement behind the original change is not settled: it
			// was made by reading a pane, and a composer holding only its own
			// faint placeholder produces exactly the reading "nothing
			// happened, the text is still there". What justifies this edit is
			// consistency, not that number — three submit sites, one shape,
			// and no reason for this one to differ. The cost either way is a
			// trailing space.
			//
			// #101: attribute whatever collapsed-paste marker is currently
			// sitting in the composer to OUR text, using an empty `before` so
			// any marker already present counts as "gained" — there is no
			// fresh paste here to distinguish it from, the paste already
			// happened on the attempt that stranded it. This is the same
			// attribution confirmLanded already performs for a fresh paste;
			// reused here for a paste that landed earlier.
			//
			// #109: `landed` IS now checked, and this reverses #101's own
			// reasoning for ignoring it. composerText (above) proves the
			// composer holds SOMETHING, not that it holds a SETTLED, complete
			// copy of our text — a very large multi-line paste can still be
			// mid-collapse, or the composer can hold two ambiguous markers
			// (an unrelated stranding alongside ours), and #101's comment
			// waved both off as "a failed re-match here only degrades atCount
			// to 0, which confirmSubmitted already treats as fall back to the
			// composer-empty check alone" — true of the OUTCOME confirmSubmitted
			// reports, but that reasoning only covers whether the SUBMIT is
			// confirmed, not whether what gets submitted is complete. Pressing
			// the wake key regardless of `landed` submits whatever is
			// currently sitting in the composer even when this driver has NO
			// attributable evidence it is our full delivery — measured live as
			// a truncated, tail-only fragment reaching the agent (#109): the
			// resume path was completing an unconfirmed, possibly-partial
			// landing instead of redoing it. `landed=false` now refuses to
			// press submit at all, the same discipline the first-attempt path
			// (below) already applies to identical evidence — the record is
			// kept either way, so a later resume gets another chance once the
			// paste has actually settled.
			// digestVerified (#180 review fix): resumeRecord.ComposerDigest
			// is either empty (this driver never corroborated the composer's
			// content at strand time) or, by construction of the mismatch
			// check just above, equal to curDigest — a mismatch already
			// returned. So a non-empty recorded digest here IS a verified
			// match, and confirmLandedV2's own suffix rule may safely be
			// reintroduced for THIS resume alone (#180 review's own
			// fix for the real, tall-composer tail-only render #143/M2
			// reproduces) — every other fallback strict=true disables stays
			// disabled regardless.
			digestVerified := resumeRecord.ComposerDigest != ""
			resumeCheck := landCheck{
				strict: true, digestVerified: digestVerified, sessionID: ref.ID,
				ownMarker: resumeRecord.pasteKey(), ownMarkerOK: resumeRecord.PasteLanded,
			}
			land := d.landV2(ctx, target.paneID, text, resumeCheck)
			key, atCount, landed := land.key, land.atCount, land.ok
			if !landed {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason: d.withRestartNoteReason(ref.ID, "resumed a delivery this driver had "+
						"stranded earlier, but the composer could not be confirmed to hold a "+
						"complete, attributable copy of it this time either — pressing submit now "+
						"risks completing a partial paste instead of the full one; the record is "+
						"kept — retry the same send with resumeIfStranded again"),
				}, nil
			}
			// Review fix (transcript offset taken after Enter): resolve the
			// transcript source, and with it the byte offset a match must
			// come AFTER, BEFORE the submit keystroke below — not after, as
			// confirmSubmittedV2 used to do internally. The runtime is
			// measured to write its own transcript entry 0.1-0.6s after the
			// keystroke; resolving the source afterwards (which itself costs
			// a pane re-list plus a `ps` call on the pid-fallback path) could
			// lose that race on a loaded host, missing a fast write and
			// reporting unknown for a delivery that actually landed —
			// exactly the false negative that then triggers a duplicate
			// resume. See resolveTranscriptSource / confirmSubmittedFromSource.
			src, srcOK := d.resolveTranscriptSource(ctx, ref, target)
			// Review fix (#180 review): resolveTranscriptSource itself
			// widens the window between the landed check above and the
			// submit keystroke below (list-panes plus a batched capture,
			// then `ps`, then file reads — measured 26.8-52.9ms on a private
			// 25-pane tmux server); nothing re-checked whether a selection
			// menu appeared in that gap before pressing Enter, so a modal
			// painted there was approved instead of receiving this
			// delivery's own submit. One more cheap capture, immediately
			// before send-keys, closes it: refuse (keep the record) rather
			// than press Enter into a screen this driver has not looked at
			// since resolving the transcript source.
			if v := d.preSubmitCheck(ctx, target, text, resumeCheck); v != preSubmitOK {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason:  d.withRestartNoteReason(ref.ID, preSubmitReason(v, true)),
				}, nil
			}
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "Space", "C-m"); err != nil {
				return fleet.DeliveryReceipt{}, fmt.Errorf("send: submitting stranded text: %w", err)
			}
			// #101: this used to report `submitted` — the strongest outcome in
			// the enum — and call forgetStranded immediately afterwards,
			// discarding this driver's own record of the text on the strength
			// of a keystroke nobody checked. Getting `unknown` on a resume
			// left the caller exactly where it started, unable to tell a
			// transient artefact from the strand this driver already counts.
			//
			// CORROBORATING WHICH TEXT IS OURS still goes through this
			// driver's own record (strandedMatches, above) rather than
			// re-reading the screen — #49 still holds, a collapsed multi-line
			// paste cannot be compared byte for byte. WHETHER THE SUBMIT
			// REGISTERED is a different question, and it is answered the same
			// evidence-based way the first-attempt path already answers it
			// below: the composer emptying, or this delivery's own attributed
			// marker clearing. Delivering a receipt this driver did not earn
			// is the exact conflation delivery.go's OutcomeSubmitted doc
			// forbids.
			confirmed, evidence, signal, turnProven := d.confirmSubmittedFromSourceTurn(ctx, target, text, key, atCount, src, srcOK)
			tr.confirmedBy = signal
			if !confirmed {
				// The record is KEPT, deliberately unlike the old behaviour: a
				// swallowed keystroke here must leave something for a third
				// attempt to resume, not discard the only trace of the text on
				// a keystroke nobody checked.
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeUnknown,
					Reason: d.withRestartNoteReason(ref.ID, "resumed a delivery this driver had "+
						"stranded earlier, but the submit could not be confirmed this time "+
						"either ("+evidence+"); the record is kept — retry the same send with "+
						"resumeIfStranded again"),
				}, nil
			}
			d.forgetStranded(ref.ID)
			if !turnProven {
				d.noteProvisional(ref.ID, target.cwd, text, src, srcOK) // #240
			}
			tr.resumed = true
			return fleet.DeliveryReceipt{
				// Queued, not submitted — matching the first-attempt path's
				// own outcome for the identical evidence (§4.3's
				// ConfirmsDelivery stays false for the same reason on both
				// paths): a confirmed submit says the bytes left this
				// driver's hands, not that the agent consumed them.
				Outcome: fleet.OutcomeQueued,
				Reason: "resumed a delivery this driver had stranded earlier, and the submit " +
					"registered this time; agent receipt is not observable on this substrate",
			}, nil
		}

		// muster #112: the resume branch just above only ever fires for
		// an EXACT match (strandedMatches) with ResumeIfStranded set. Every
		// other shape used to fall straight through to one refusal claiming
		// "text a human typed" — even when this driver's OWN record said
		// otherwise. Consult that record now, for the three cases that
		// refusal was collapsing into one.
		if record, hasRecord := d.strandedRecordFor(ref.ID, target.cwd); hasRecord {
			if opts.ReplaceIfStranded {
				// The draft rule (#180): the record must still prove the
				// composer holds exactly its text, or the caller's expect
				// digest must match the composer as it is now.
				if !recordProves(record, pending, screenNow) && !expectProves(opts.ExpectComposerDigest, pending) {
					tr.draftKept = true
					return fleet.DeliveryReceipt{
						Outcome: fleet.OutcomeRefused,
						Reason: "composer holds a delivery this driver made into this session, but its " +
							"content has changed since that delivery was recorded and this driver " +
							"cannot confirm it is still only its own text — it may now include a " +
							"person's edits, and it was kept. Pass expect=" + composerTextDigest(pending) +
							" with replaceIfStranded to clear exactly what is there now, or discard the " +
							"composer directly (discard?expect=" + composerTextDigest(pending) + ")",
					}, nil
				}
				expectedLines, _ := composerVisualLines(screenNow)
				receipt, cleared, err := d.tryReplaceStranded(ctx, ref, target, record, pending, expectedLines, screenNow)
				if err != nil {
					return fleet.DeliveryReceipt{}, err
				}
				if !cleared {
					return receipt, nil
				}
				// Cleared: the stranded record is already forgotten inside
				// tryReplaceStranded. Fall through — deliberately NOT a
				// return — to the ordinary delivery path below, which pastes
				// and confirms THIS call's text exactly as it would for a
				// composer that was never busy, and writes a fresh delivery
				// mark for #111 in the process.
			} else if record.Text == text {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "composer holds a delivery this driver made into this session and " +
						"could not confirm submitted; it is this service's own text, not a " +
						"person's draft. Resend the same text with resumeIfStranded to finish it",
				}, nil
			} else {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "composer holds a delivery this driver made into this session and " +
						"could not confirm submitted — this driver's own record says the text " +
						"there is its own, not a person's draft — but the text being sent now " +
						"is different. resumeIfStranded finishes the ORIGINAL delivery; " +
						"replaceIfStranded discards it and delivers this text instead; or " +
						"discard the composer directly first (discard?expect=" +
						composerTextDigest(pending) + ")",
				}, nil
			}
		} else if opts.ReplaceIfStranded || opts.ResumeIfStranded {
			// No live record for this composer. The draft rule (#180): clear
			// it and deliver this call's text (muster #135's door) only
			// on proof the text there is this driver's own — a tombstone of
			// a lapsed record — or the caller's expect digest matching it
			// now. Before #180, replaceIfStranded alone was taken as proof
			// and a person's draft was cleared with nothing but the send
			// grant (M9); resumeIfStranded alone had been refused outright,
			// which also refused #135's own case, a retry that outlived its
			// record (M2).
			flag := "replaceIfStranded"
			if opts.ResumeIfStranded {
				flag = "resumeIfStranded"
			}
			proof, mismatch := d.mayClearUnrecorded(ref.ID, target.cwd, pending, screenNow, opts.ExpectComposerDigest)
			if proof == proofTombstone {
				// #240: was it a provisional entry — a delivery reported queued, now back?
				if _, provisional := d.tombstoneProvesKind(ref.ID, target.cwd, pending, screenNow); provisional {
					d.counters.incr(counterStrandedProvisionalHandedBack)
				}
			}
			if proof == proofNone {
				d.noteUnexplainedLabelled(ref.ID, target.cwd, pending)
				tr.draftKept = true
				if opts.ResumeIfStranded {
					d.counters.incr(counterSendRefusedResumeNoRecord)
				}
				digest := composerTextDigest(pending)
				reason := "the composer holds text this driver has no record of placing, so it may " +
					"be a person's draft; " + flag + " alone does not prove otherwise, and the text " +
					"was kept. Read the session, and if the text there is yours to replace, send " +
					"again with replaceIfStranded and expect=" + digest + " — the composer's current " +
					"digest — or discard it directly (discard?expect=" + digest + ")"
				if mismatch {
					reason = "the expect digest on this call does not match the composer as it is " +
						"now (" + digest + "): it changed after it was read, possibly because a " +
						"person typed into it, and it was kept. Read the session again before " +
						"deciding to replace it"
				}
				return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
			}
			expectedLines, _ := composerVisualLines(screenNow)
			receipt, cleared, clearErr := d.tryClearUnrecordedComposer(ctx, ref, target, pending, expectedLines, screenNow, opts)
			if clearErr != nil {
				return fleet.DeliveryReceipt{}, clearErr
			}
			if !cleared {
				return receipt, nil
			}
			// Cleared on proof: fall through — deliberately NOT a return — to
			// the ordinary delivery path below.
		} else {
			tr.draftKept = true
			// #240: no live record, but a delivery this driver reported queued
			// may have been handed back. Say whose it is, so the sender need not
			// read the screen to find out.
			if proved, provisional := d.tombstoneProvesKind(ref.ID, target.cwd, pending, screenNow); proved {
				if provisional {
					d.counters.incr(counterStrandedProvisionalHandedBack)
				}
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeRefused,
					Reason: "composer holds a message this driver delivered into this session and " +
						"reported queued; the runtime has handed it back unsent, so it is this " +
						"service's own text, not a person's draft (strandedDelivery on the session's " +
						"state). Send the same text again with resumeIfStranded to finish it, or " +
						"replaceIfStranded to discard it and deliver this text instead; nothing was written",
				}, nil
			}
			d.noteUnexplainedLabelled(ref.ID, target.cwd, pending)
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "composer holds unsent input; delivering would concatenate " +
					"with text a human typed and has not submitted (§2.4). If it is text this " +
					"driver left there, resumeIfStranded finishes it; if it is yours to replace, " +
					"send again with replaceIfStranded and expect=" + composerTextDigest(pending) +
					" — the composer's current digest — or discard it directly first " +
					"(discard?expect=" + composerTextDigest(pending) + ")",
			}, nil
		}
	}

	// The composer's marker state, read immediately before this delivery's
	// own paste — not reused from screenNow above, which was captured before
	// the pending-composer gate and can be stale by however long that check
	// took. Everything confirmLandedV2 and confirmSubmittedV2 attribute to
	// THIS delivery is a CHANGE relative to this snapshot; see markerCounts
	// for why the presence of a marker was never enough on its own.
	// #180 H2: the pane's foreground process must be the runtime before
	// anything is pasted — a shell under a composer frame the runtime left
	// behind reads as an empty composer, and would receive the text.
	if ok, why := d.foregroundIsRuntime(ctx, target); !ok {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  why + "; nothing was pasted",
		}, nil
	}

	before := map[pasteKey]int{}
	if sc, ok := d.captureForClassify(ctx, target.paneID); ok {
		before = composerMarkers(sc)
	}

	// Terminal path v2 / D2+D3: bracketed, literal-newline delivery
	// (pasteBracketed, terminalpath2.go) replaces the plain, CR-converting
	// `paste-buffer -d` call this line used to make directly — see
	// pasteBracketed's own doc comment for the defect this closes and why a
	// refusal, not a silent fallback to the old call, is what happens when
	// bracketed paste cannot be confirmed available.
	if err := d.pasteBracketed(ctx, target.paneID, text); err != nil {
		if errors.Is(err, errBracketPasteUnavailable) {
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: err.Error()}, nil
		}
		return fleet.DeliveryReceipt{}, err
	}

	// muster #111: this is the single write site for a delivery mark —
	// the moment "a delivery was made into this composer" becomes true.
	// Every outcome below (queued unsubmitted, stranded-unknown, confirmed)
	// shares this one mark, and a resume completing an EARLIER delivery
	// never reaches this line at all, so `turns` never resets under it.
	//
	// muster #222: EXCEPT a session-management command (/rename, /rc,
	// /remote-control). Those produce no agent turn at all, so marking
	// `turns` for one would read as "a delivery was made and nothing has
	// completed since" — the false work-lost signal #111 exists to
	// prevent — which stopped being a rare /input edge case the moment
	// #222 made a `/rename` delivery a guaranteed side effect of every API
	// rename.
	if !isSessionCommand(text) {
		d.noteDelivery(ref.ID, target.cwd)
	}

	if !opts.Submit {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeQueued,
			Reason:  "placed in the composer, not submitted",
		}, nil
	}

	// CONFIRM THE TEXT LANDED BEFORE SUBMITTING, and submit with C-m.
	//
	// Both halves come from a sibling project that measured this family of
	// failures over months, and both are the difference between "the message
	// arrived" and "the message was received".
	//
	// The race: pasting and submitting back-to-back lets the submit win, so
	// the prompt is submitted EMPTY and the text lands a moment later — where
	// it then sits unsent forever. That signature was counted at eight
	// stranded operator instructions in a single day, and separately at 37 of
	// 39 panes fleet-wide.
	//
	// The keystroke: `Enter` was observed being silently dropped on a pane
	// where `C-m` submitted immediately, same text, seconds apart. They are
	// the same character in principle; they are not the same in practice, and
	// only one of them has been seen to work when the other did not.
	freshCheck := landCheck{before: before, sessionID: ref.ID}
	land := d.landV2(ctx, target.paneID, text, freshCheck)
	key, atCount, landed := land.key, land.atCount, land.ok
	if !landed {
		// The text is in the composer and was not submitted. Say so plainly:
		// the caller must decide whether to retry or clear it, and silence
		// here is how a session ends up holding an instruction nobody knows
		// about.
		//
		// Two established facts, kept apart on purpose: no literal prefix
		// rendered, AND no single new paste marker could be pinned on this
		// delivery alone — either nothing has landed yet, or something
		// landed at the same moment as an unrelated change and this driver
		// will not guess which is ours. Either way the honest instruction is
		// the same as before.
		// Record what we left behind, so the caller has a way to finish this
		// rather than being told where the text is and left there. Also
		// resolves and records a transcript source (#180 review fix)
		// so a LATER resumeIfStranded finding this composer empty can check
		// whether the runtime accepted it in the meantime instead of
		// re-pasting blind.
		strandSrc, strandSrcOK := d.resolveTranscriptSource(ctx, ref, target)
		// #180 M1: nothing rendered and no Enter was pressed — the one class
		// a later resume may paste again when it finds the composer empty.
		strandDigest := d.currentComposerDigest(ctx, target.paneID)
		d.noteStrandedLanding(ref.ID, target.cwd, text, strandDigest, strandSrc, strandSrcOK, land, strandDigest == "")
		if land.preempted {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: d.withRestartNoteReason(ref.ID, "text was pasted, but a respond to this "+
					"session arrived before it could be confirmed in the composer and was given "+
					"priority; submit was not pressed. The text may be sitting in the composer "+
					"unsent — once the respond is done, retry the same send with resumeIfStranded "+
					"to submit it"),
			}, nil
		}
		if land.dialog {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: d.withRestartNoteReason(ref.ID, "text was pasted, but a selection menu "+
					"appeared on this pane before it could be confirmed in the composer; submit "+
					"was not pressed, so nothing answered that menu. The text may be sitting in "+
					"the composer unsent — once the menu is answered, retry the same send with "+
					"resumeIfStranded to submit it"),
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: d.withRestartNoteReason(ref.ID, "text was delivered to the composer but "+
				"did not render in time to be confirmed landed, and no single new paste "+
				"marker could be attributed to this delivery alone; it may be sitting there "+
				"unsent — retry the same send with resumeIfStranded to submit it"),
		}, nil
	}
	// The wake key: `Space` before the newline, in ONE send-keys call.
	//
	// The FIRST keystroke into an idle pane is swallowed when that keystroke is
	// Enter — measured 6 times out of 6 on real sessions. A printable key in
	// the same position is not swallowed, and once it has landed the pane is no
	// longer idle, so the newline that follows it submits.
	//
	// A paste is not a keystroke. So after paste-buffer the submit is ALWAYS
	// the first keystroke, which means a lone newline here hits the failing
	// case on every delivery into a pane that has gone idle — most of them.
	// The confirmation above proves the text RENDERED; it does not make the
	// submit land, and the two failures look identical from outside: a receipt
	// that says submitted and a composer still holding the line.
	//
	// Both keys go in one invocation because they are both key names — no `-l`
	// — and because a second call would reintroduce a race between them.
	//
	// The trailing space is accepted as harmless: a submitted line one space
	// longer changes nothing downstream. Do NOT tidy it with a `BSpace` before
	// the newline — that puts a non-printable key back in the first-keystroke
	// slot, which is precisely the untested case.
	//
	// Review fix (transcript offset taken after Enter — see the resume site's
	// identical fix above for the full reasoning): resolve the transcript
	// source, and the byte offset that goes with it, BEFORE this keystroke,
	// not after.
	src, srcOK := d.resolveTranscriptSource(ctx, ref, target)
	// Review fix (#180 review): the same dialog-race close as the resume
	// site above — one cheap, fresh capture immediately before send-keys,
	// refusing (and recording the stranded text with its own transcript
	// source, exactly as the timeout path just above does) rather than
	// pressing Enter into a screen this driver has not looked at since
	// resolveTranscriptSource's own list-panes/`ps`/file-read window opened.
	if v := d.preSubmitCheck(ctx, target, text, freshCheck); v != preSubmitOK {
		d.noteStrandedLanding(ref.ID, target.cwd, text, d.currentComposerDigest(ctx, target.paneID), src, srcOK, land, false)
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason:  d.withRestartNoteReason(ref.ID, preSubmitReason(v, false)),
		}, nil
	}
	if _, err := d.run(ctx, d.bin, "send-keys", "-t", target.paneID, "Space", "C-m"); err != nil {
		// #192: the paste was CONFIRMED in the composer just above, so a
		// keystroke that fails outright leaves this delivery's text sitting
		// there, unsent as far as this driver can tell (a call that died on its
		// own deadline may still have delivered the keys) — the same state as
		// every other way the submit goes wrong below and above this line, and
		// it must leave the same record.
		// Returning the error alone left the composer holding text nothing
		// remembered: a follow-up auto send saw no unconfirmed terminal
		// delivery (terminalUnconfirmed) and took the inbox, delivering the
		// text once as a peer message with a copy still waiting to be
		// submitted, and a resumeIfStranded retry was refused for want of a
		// record. The record is what both of those, and #184's route guard,
		// read. Noted before returning, with the transcript source resolved
		// before the keystroke so a later resume can still ask the runtime
		// whether it took the text in the meantime.
		//
		// The composer digest is read AFTER the failure and is empty when that
		// read fails too (the usual case when tmux itself is gone); an empty
		// digest is the documented honest degrade — resume then falls back to
		// the id+cwd+text match and its own live re-verification.
		d.noteStrandedLanding(ref.ID, target.cwd, text, d.currentComposerDigest(ctx, target.paneID), src, srcOK, land, false)
		return fleet.DeliveryReceipt{}, fmt.Errorf("send: submitting (the text may be sitting in the composer "+
			"unsent, and a stranded record was kept for it — retry the same send with resumeIfStranded to submit it): %w", err)
	}

	// Queued, not submitted: see Capabilities. The bytes were handed to
	// the substrate; whether the agent consumed them is not observable
	// here, and claiming otherwise is the emulation §5.6 forbids.
	// CONFIRM THE SUBMIT REGISTERED, by watching the composer empty OR by
	// watching this delivery's own attributed marker clear — see
	// confirmSubmitted for why the second path had to be added.
	//
	// Everything before this point is inference about whether a keystroke
	// would be received; this is evidence about whether it was. Without it a
	// dropped submit is indistinguishable from a delivered one — same receipt,
	// same silence — and the caller learns about it, if ever, from a session
	// that mysteriously never answers.
	//
	// Recorded as stranded so the resumeIfStranded path can finish it. That
	// path previously could not reach this class of failure at all: the record
	// was only written when the text failed to RENDER, so a submit that went
	// nowhere left nothing behind, the resume was refused for lack of a
	// record, and every later send was refused for a busy composer.
	submitConfirmed, submitEvidence, submitSignal, submitTurnProven := d.confirmSubmittedFromSourceTurn(ctx, target, text, key, atCount, src, srcOK)
	tr.confirmedBy = submitSignal
	if !submitConfirmed {
		d.noteStrandedLanding(ref.ID, target.cwd, text, d.currentComposerDigest(ctx, target.paneID), src, srcOK, land, false)
		if strings.HasPrefix(submitEvidence, differentTurnComposerEmptied) {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: d.withRestartNoteReason(ref.ID, "the text landed and a submit was issued; "+
					submitEvidence+". Read the session before sending it again"),
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: d.withRestartNoteReason(ref.ID, "the text landed and was attributed to "+
				"this delivery, and a submit was issued, but this delivery's own block did "+
				"not clear and the composer did not empty — the submit did not register for "+
				"it ("+submitEvidence+"). It is sitting there unsent; retry the same send with "+
				"resumeIfStranded to submit it"),
		}, nil
	}

	// Terminal path v2 / item e: a confirmed send clears any stranded record
	// this driver was still holding for this session — whatever it recorded
	// no longer describes the composer's current, just-confirmed state (see
	// Discard's own two Accepted:true returns for the matching fix there).
	d.forgetStranded(ref.ID)

	// #240: a confirmation that does not show a turn started leaves the text
	// possibly still in the runtime's queue, and a queue can be handed back to
	// the composer — unsent, with this driver's memory of the delivery just
	// dropped above. Keep a provisional record so that, if the text comes
	// back, it is known to be this driver's own and its sender can resume it.
	reason := "text rendered in the composer and the submit registered; " + submitEvidence
	if !submitTurnProven {
		d.noteProvisional(ref.ID, target.cwd, text, src, srcOK)
		reason += ". This shows the runtime took the text off the composer, not that it " +
			"started a turn on it: if it is handed back to the composer unsent (a queued " +
			"message returned after the running turn was interrupted or failed), the session " +
			"will report waiting_input with strandedDelivery true, and sending the same text " +
			"again with resumeIfStranded finishes it"
	}

	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeQueued,
		// Still queued, not submitted: an emptied composer says the SUBMIT
		// took, not that the agent consumed the input — and §4.3's
		// ConfirmsDelivery stays false for exactly that distinction. What the
		// confirmation buys is that this receipt no longer covers the case
		// where nothing was submitted at all.
		Reason: reason,
	}, nil
}

// Interrupt asks a session to stop what it is doing (§3). It expresses
// intent only — the Ack says the request was accepted, never that the agent
// stopped; that arrives later as a state change (§2.5).
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	for _, r := range rows {
		if r.session == ref.ID {
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", r.paneID, "Escape"); err != nil {
				return fleet.Ack{}, fmt.Errorf("interrupt: %w", err)
			}
			return fleet.Ack{Accepted: true}, nil
		}
	}
	return fleet.Ack{}, ErrNotFound
}

// Close destroys a session (§3) — and is the operation where §5.4 does not
// survive the interface it is specified against.
//
// §5.4 requires corroborating "at least one independent attribute (working
// directory, start time, name)" before acting destructively, because ids
// are recyclable. But close() receives only a SessionRef, which carries
// machine, id and a human label. There is no field in which a req can
// say *which* session it means beyond the id — so a driver has nothing to
// compare the live session against except its own earlier sighting.
//
// That is what this implements, and it is worth being precise about what it
// does and does not buy:
//
//   - CLOSED: the window between this driver observing a session and being
//     asked to destroy it. If the id was recycled in that interval, the
//     start time will differ and this refuses.
//   - OPEN, and not closable here: the window between the *caller*
//     observing a session and calling close. The caller's evidence never
//     reaches the driver. A req that listed sessions, went away, came
//     back after a recycle and called close gets no protection from this
//     check, because the driver's own sighting may have been refreshed in
//     the meantime.
//
// Closing the second window needs SessionRef to carry a corroborating
// attribute — a start time the req observed — so that close can compare
// against the caller's belief rather than the driver's. That is a change to
// the specification, not to this file; it is recorded in the package doc's
// FINDINGS and in the spec's §5.4 (open defect D2, §14).
//
// A ref this driver has never seen is refused outright rather than
// destroyed on an id match, which is the literal thing §5.4 forbids.
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.Ack{}, ErrNotFound
	}

	// The strong guarantee: compare against what the CALLER observed.
	//
	// This is the window that matters. A driver comparing against its own
	// last sighting only proves nothing changed since the driver looked,
	// which says nothing about the interval the caller has been away — and
	// across a network that interval contains a round trip at minimum.
	if want := req.Expect.StartedAt; want != nil {
		if !live.created.Equal(*want) {
			return fleet.Ack{}, fmt.Errorf(
				"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
				ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		return d.killCorroborated(ctx, ref)
	}

	// The weak guarantee, applied only when the caller supplied nothing to
	// corroborate against — and named as weak in any refusal, so nobody
	// mistakes it for the rule above. It closes the window between this
	// driver's own sighting and now, which is better than an id match and
	// less than §5.4 asks for.
	d.mu.Lock()
	prior, seen := d.observed[ref.ID]
	d.mu.Unlock()
	if !seen {
		return fleet.Ack{}, fmt.Errorf(
			"%w: caller supplied no expected start time, and this driver has no prior "+
				"observation of id %q either; nothing corroborates the target",
			ErrAmbiguousTarget, ref.ID)
	}
	if !live.created.Equal(prior.created) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q was recycled since this driver last observed it (weak check: "+
				"the caller supplied no expected start time)",
			ErrAmbiguousTarget, ref.ID)
	}
	if live.cwd != prior.cwd {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q now has working directory %q, not %q (weak check)",
			ErrAmbiguousTarget, ref.ID, live.cwd, prior.cwd)
	}
	return d.killCorroborated(ctx, ref)
}

// Discard clears unsent composer text without submitting it (§3).
//
// # Why this verb had to exist
//
// `send` refuses to append to a composer that already holds something (§2.4),
// which is right — appending corrupts somebody's line. But that refusal left a
// caller with nowhere to go: the only operations that touch a busy composer
// were "submit it" and "destroy the session holding it", and neither is safe
// for text the caller did not write.
//
// It arrived from real use twice over. A fleet was found holding operator text
// unsent for hours, and separately a supervisor's own keepalive stranded lines
// it never meant to send. Both needed removal, and removal did not exist.
//
// # It destroys typing, so it corroborates like a destroy
//
// expectDigest is what the caller last saw in ComposerDigest. A mismatch means
// the composer changed since — most likely a human typing this second — and
// deleting then would destroy something nobody has looked at. Refused, with
// the same sentinel `close` uses for the same reason.
//
// Discarding blind (no digest) is refused outright rather than treated as
// permission. "I do not know what is there, remove it" is exactly the request
// this operation must not honour.
//
// # An empty composer is a success
//
// A caller that timed out and retried must not be told it failed for having
// worked the first time. Nothing is destroyed by clearing nothing.
//
// # Force is the exit from the cycle discardProvenFutile used to dead-end at
//
// muster#136: before this, a residue the ordinary pass had already
// proven futile left exactly one documented remedy — DELETE the session.
// Disproportionate: a session carries a conversation, a bridge, in-flight
// work, and (for a caller that binds them) a claim and a worktree, none of
// which respawning recovers. opts.Force, set only once futileClearAttempts
// has already fired for this residue, reaches for clearComposerSweep
// instead — a character-budgeted Backspace sweep past the ordinary pass's
// structural key choice. It never relaxes expectDigest: the corroboration
// above runs identically whether Force is set or not, because a forced
// clear is MORE destructive, not less.
func (d *Driver) Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string, opts driver.DiscardOptions) (fleet.Ack, error) {
	// Terminal path v2 / D4 — see Send's identical acquisition for why this
	// is the very first thing every composer-touching entry point does, and
	// lockComposerOpsCtx's own doc comment for why the caller's ctx (not a
	// bare, uninterruptible Lock()) governs the wait.
	unlockComposer, lockOK := d.lockComposerOpsCtx(ctx, ref.ID)
	if !lockOK {
		return fleet.Ack{}, fmt.Errorf("discard: %s", composerBusyReason(""))
	}
	defer unlockComposer()

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.Ack{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.Ack{}, d.noSuchSession(ctx, rows, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !live.created.Equal(*want) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	sc := captures[live.paneID].screen()
	pending, scan := composerText(sc)
	// muster#215, moved ahead of the clipped refusal by #216: the feedback-
	// draft card over a composer this driver could not read as a whole. A
	// composer cut off by the bottom of the pane now reads clipped where it read
	// absent, so the card would otherwise be met by the generic refusal, whose
	// words (a taller-than-the-window composer) are wrong for it. It is refused
	// whatever the row holds: "already clear" is the answer this verb gives for
	// any composer it did not find, and for a row it could not read it is false.
	if scan != composerFound {
		if c, ok := liveFeedbackCard(sc); ok && !c.composerFound {
			d.counters.incr(counterFeedbackCardRefusedDiscard)
			reason, _ := feedbackCardRefusal(sc)
			return fleet.Ack{}, fmt.Errorf("%w: discard: %s", ErrAmbiguousTarget, reason)
		}
		// muster#217: the feedback panel replaces the composer outright, so
		// "already clear" would be a claim about a composer that is not on screen.
		// Asked here, ahead of the clipped refusal, whatever scan says: a panel
		// reads absent today, and a screen that read it clipped would otherwise be
		// met by a refusal whose words (a composer taller than the window) are
		// wrong for it.
		if reason, ok := feedbackPanelRefusal(sc); ok {
			d.counters.incr(counterFeedbackCardRefusedDiscard)
			return fleet.Ack{}, fmt.Errorf("%w: discard: %s", ErrAmbiguousTarget, reason)
		}
	}
	if scan == composerClipped {
		// muster#134: this driver's capture ended above the composer's
		// opening fence. Claiming "already clear" here is #134's own false
		// negative one function over — this driver has not seen enough of
		// the composer to say that honestly, so it must not.
		//
		// muster#149: and nothing below this point may be tried in its
		// place — no wider capture, no flag, no caller say-so. None of them is
		// evidence about the rows this driver cannot see; see
		// docs/adr/149-a-clipped-composer-has-no-in-driver-proof.md. The
		// refusal is counted so its rate is readable, which is the evidence
		// that ADR's reopen condition asks for.
		d.counters.incr(counterComposerClippedRefusedDiscard)
		d.countClippedCause(sc)
		return fleet.Ack{}, discardComposerClipped(sc)
	}
	if pending == "" {
		// Already clear — including the case where what looked like text was
		// the dim placeholder, which is not text at all and never was.
		//
		// Terminal path v2 / item e, review-narrowed: forget any stranded
		// record this driver holds for this session, but ONLY when scan
		// actually confirmed a composer and found it empty (composerFound) —
		// never on composerAbsent. A screen with NO fenced composer at all
		// is not proof the composer emptied: it is exactly the shape a
		// full-screen dialog paints (composerSpan's own doc comment), and a
		// dialog covering this driver's own still-stranded text is not the
		// same fact as that text having gone away. Forgetting the record
		// there would lose the only trace of an undelivered message the
		// moment the dialog closes and the composer reappears holding it.
		// composerText's own contract already tells the two findings apart
		// (composerAbsent is POSITIVE — "structurally no composer" — but
		// that is a fact about the SCREEN, not a corroborated fact about
		// what this driver's own stranded record describes); this is the
		// first caller that had to act on the distinction rather than just
		// pass composerClipped through unchanged.
		if scan == composerFound {
			d.forgetStranded(ref.ID)
		}
		return fleet.Ack{Accepted: true}, nil
	}
	if expectDigest == "" {
		return fleet.Ack{}, fmt.Errorf(
			"%w: refusing to discard %d characters the caller has not seen; "+
				"supply the composerDigest from a read as ?expect=<composerDigest> "+
				"(a query parameter, the same place startedAt goes — not a body field, "+
				"even though composerDigest is the name of a field IN the read response)",
			ErrAmbiguousTarget, len(pending))
	}
	if got := composerTextDigest(pending); !composerDigestMatches(expectDigest, pending) {
		return fleet.Ack{}, fmt.Errorf(
			"%w: the composer holds different text than the caller saw "+
				"(expected digest %s, found %s) — somebody may be typing right now",
			ErrAmbiguousTarget, expectDigest, got)
	}

	// #87: this EXACT residue may already be proven to resist a clear pass —
	// a prior call spent a full, content-sized budget (clearComposer's own
	// expectedLines+clearPressMargin, capped at maxClearPresses) pressing
	// C-u against it and it never moved. If so, do not press again: a
	// second identical pass against text nobody has re-read is not
	// gathering more evidence, it is destructive keystrokes spent to learn
	// what the first pass already learned.
	//
	// #129 asked whether this still holds now that a real human's
	// persistence was observed clearing a composer this exact refusal had
	// given up on, and the answer is: refusing here is unchanged, but the
	// PASS this now guards is not the one that field case indicted. That
	// pass used to be bounded by a flat 3-second clock regardless of what
	// the composer held — a paste sized like #129's own field case could
	// not have finished inside it at any machine speed, which is a
	// different failure than "pressing again would not help." Now that a
	// pass is sized to the composer's own row count instead, a residue that
	// still has not moved after that many presses is evidence a human's
	// persistence never actually contradicted: nothing here claims a human
	// pressing MORE than a content-sized pass already did would fail too,
	// only that this driver pressing the SAME text again, having already
	// spent that pass, would not learn anything new. Refuse outright,
	// before touching the pane.
	if attempts := d.futileClearAttempts(ref.ID, live.cwd, expectDigest); attempts > 0 {
		if !opts.Force {
			return fleet.Ack{}, d.withRestartNote(ref.ID, discardProvenFutile(attempts))
		}
		// muster#136: Force is the escape hatch discardProvenFutile's
		// own refusal now names, reached only because attempts>0 — the
		// ordinary pass HAS already been tried and found futile against
		// this exact residue. expectDigest was already corroborated above,
		// unconditionally; Force does not relax that, it only authorises a
		// stronger mechanism once corroboration has already passed.
		left, cleared, sweepErr := d.clearComposerSweep(ctx, live.paneID, ref.ID, pending)
		if sweepErr != nil {
			return fleet.Ack{}, fmt.Errorf("discard: %w", sweepErr)
		}
		if cleared {
			// Terminal path v2 / item e.
			d.forgetStranded(ref.ID)
			d.counters.incr(delivery.CounterPrefix + d.deliveryModule().Name() + "." + string(delivery.Discarded))
			return fleet.Ack{Accepted: true}, nil
		}
		return fleet.Ack{}, d.withRestartNote(ref.ID, discardIncomplete(pending, left))
	}

	expectedLines, _ := composerVisualLines(sc)
	left, _, cleared, err := d.clearComposer(ctx, live.paneID, ref.ID, live.cwd, pending, expectedLines, sc)
	if err != nil {
		return fleet.Ack{}, fmt.Errorf("discard: %w", err)
	}
	if cleared {
		// Terminal path v2 / item e.
		d.forgetStranded(ref.ID)
		d.counters.incr(delivery.CounterPrefix + d.deliveryModule().Name() + "." + string(delivery.Discarded))
		return fleet.Ack{Accepted: true}, nil
	}
	return fleet.Ack{}, d.withRestartNote(ref.ID, discardIncomplete(pending, left))
}

// Composer reports what text is sitting unsent in a session's composer, with the
// digest Discard accepts for exactly that text (muster #276).
//
// It reads the same screen Discard reads, through the same composerText, and
// digests it with the same composerTextDigest State publishes as
// ComposerDigest — so a read followed by a discard agree by construction, and a
// composer that changes between the two is refused by Discard's own digest check
// rather than silently destroyed.
//
// It takes no composer lock: it presses nothing, so it cannot race a
// delivery into corrupting anything, and a read that lands mid-delivery returns
// the composer as it stood, which is all a snapshot ever promised.
//
// A composer this driver cannot read as a whole is a refusal and never an empty
// answer. "Nothing is there" would be a claim about text the driver has not seen
// (§5.7), and for a supervisor reporting stranded text a false empty is worse
// than an error.
func (d *Driver) Composer(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.ComposerRead, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.ComposerRead{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.ComposerRead{}, d.noSuchSession(ctx, rows, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !live.created.Equal(*want) {
		return fleet.ComposerRead{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	sc := captures[live.paneID].screen()
	pending, scan := composerText(sc)
	if scan != composerFound {
		if reason, ok := feedbackCardRefusal(sc); ok {
			return fleet.ComposerRead{}, fmt.Errorf("%w: composer: %s", ErrAmbiguousTarget, reason)
		}
		if reason, ok := feedbackPanelRefusal(sc); ok {
			return fleet.ComposerRead{}, fmt.Errorf("%w: composer: %s", ErrAmbiguousTarget, reason)
		}
		return fleet.ComposerRead{}, fmt.Errorf(
			"%w: composer: this session's composer cannot be read as a whole right now (it is clipped by the "+
				"window or covered by something else), so whether text is sitting in it is not known",
			ErrAmbiguousTarget)
	}
	if pending == "" {
		return fleet.ComposerRead{}, nil
	}
	return fleet.ComposerRead{Text: pending, ComposerDigest: composerTextDigest(pending)}, nil
}

var _ driver.ComposerReader = (*Driver)(nil)

// clearComposer walks a composer's unsent text backward with repeated C-u
// presses until it empties or the pass proves futile — the mechanism both
// Discard and muster #112's replace-stranded path need, extracted here
// so the two callers cannot drift apart on #87's stall/futility semantics.
// Discard is the ORIGINAL of this code.
//
// pending is the composer text the CALLER has already corroborated —
// Discard's digest check, or #112's ComposerDigest match — BEFORE calling
// this. It presses keys unconditionally and trusts that corroboration
// already happened; it does not repeat it.
//
// expectedLines is composerVisualLines' count for that SAME corroborated
// screen, computed by the caller because it already has the screen this
// pending came from — see composerVisualLines for why an on-screen row
// count, not a duration, is what a press budget should be sized to. sc IS
// that same screen, passed through so the very first press can already be
// chosen correctly — see the #132 section below for why the choice matters
// from the first iteration, not just later ones.
//
// # Why a press count now, not muster#129's retired promptClearWindow
//
// C-u clears the line the cursor sits on: readline's unix-line-discard,
// killing from the cursor back to the start of the CURRENT line, not the
// whole buffer. A composer holding one short line empties in one press,
// measured directly (C-a C-k and Escape were tried too; C-u alone was
// enough for a single line). A composer spanning several rows does not —
// one press clears the row the cursor is on and leaves every row above it
// standing, so a single un-repeated press against a multi-row paste can
// only ever get partway there (issue #32's field case: 209 characters,
// four on-screen rows, corrupted by the un-repeated fix of the day). So
// this presses the same key again on every iteration the composer is still
// non-empty, walking it backward one row at a time — the same thing an
// operator clearing a stuck multi-line prompt by hand would do.
//
// The bound on how many times to do that used to be a flat 3-second clock
// (promptClearWindow), sized against #87's stuck-composer case and never
// checked against the opposite one: #129 measured a real paste (~6.6 KB,
// on the order of eighty on-screen rows) whose OWN required press count —
// roughly one per row — could not fit inside that budget regardless of
// machine speed. A duration is the wrong unit for work that scales with
// row count; expectedLines+clearPressMargin is presses, the unit the work
// is actually measured in, capped at maxClearPresses so the pass still
// terminates for content with no natural size limit (see that constant).
//
// # C-u alone cannot cross a real newline — muster#132
//
// unix-line-discard kills back to the start of the CURRENT line and stops
// there; it was never defined to reach past a line boundary. A payload
// ending in one or more real trailing (or interior) newlines leaves exactly
// that shape behind: a blank row, with nothing on it for C-u to kill. Every
// further C-u press against that row is a guaranteed no-op — the capture
// comes back byte-for-byte identical, which used to read as either "stopped
// moving" (#87's stall path, if some earlier row had already cleared) or
// "never moved at all" (noteFutile), permanently wedging the composer: the
// residue is recorded as proven-unclearable and every later Discard/replace
// call is refused before it ever presses a key again, even though nothing
// about the surrounding TEXT resisted clearing — only the mechanism did.
// This is the exact field shape #132 reports (a partial clear stranding
// 306 characters with no API path back).
//
// composerCursorRowBlank answers, before each press, whether the row the
// cursor is assumed to sit on (the composer's current bottom row) is
// exactly this shape. When it is, this presses Backspace instead of C-u:
// Backspace deletes the ONE character behind the cursor, which on an empty
// row is the newline itself, merging that row into the end of the row
// above it — crossing the boundary C-u cannot. The very next iteration then
// finds a non-blank (or shorter) composer and C-u resumes making progress
// against real content, exactly as before. Escape and C-a C-k were already
// measured not to help here (see discardProvenFutile's doc comment); this
// is not a third alternative to C-u, it is what runs INSTEAD of C-u for the
// one row shape C-u structurally cannot touch, alternating back to C-u the
// moment that shape is gone.
//
// A row disappearing this way is real progress even when composerText's
// own joined string does not change: composerText already drops every
// blank continuation row from what it concatenates (see its own doc
// comment), so a merge that only removes a blank row is invisible to a
// TEXT-only comparison. Comparing composerVisualLines' row count as well —
// not text alone — is what keeps that progress from being misread as "made
// no difference" by the stall (#87) and futility (#87/#129) logic below,
// which both key off whether ANY press changed anything observable.
//
// #87's early exit is UNCHANGED by any of this: once movement has actually
// been observed (by either signal), stallPresses further presses that
// change nothing are no longer "still walking it backward" — they are
// evidence the pass has stopped working, and pressing MORE just makes a
// bigger dent for no further gain. That check still fires before the press
// budget is necessarily exhausted, exactly as before; only what counts as
// movement, and which key gets pressed, have changed.
//
// # Row blankness is a PROXY for "anything behind the cursor" — muster#138
//
// composerCursorRowBlank's own doc comment names its assumption directly:
// the cursor is assumed to sit at the END of the composer's bottom row. C-u
// kills back to the start of whatever line the cursor is ACTUALLY on, so
// the proxy is only valid when that assumption holds. It stops holding the
// moment residue sits on the composer's own ❯-marked row: that row can
// never read as blank (its doc comment again — the marker glyph is always
// visible content), so curBlank is structurally false and, before #138,
// this loop pressed plain C-u against it forever. Measured directly: 305
// characters of residue, a full content-sized press budget spent, zero
// movement — the signature of a key that could never have worked on that
// shape, not merely "ran out of presses".
//
// The fix is two mechanisms, one for the ordinary non-blank case and one as
// a convergence guarantee if the first does not hold on some substrate:
//
//  1. Position before killing. When the current row is not blank, this
//     sends composerLineEndKey (End) THEN C-u, as one press-budget slot —
//     not C-u alone. End moves the cursor to wherever this row's content
//     actually ends, which is non-destructive (unlike sending Backspace
//     "just in case", rejected as an alternative to #132 already, for
//     exactly the reason it would eat a real character on a non-blank
//     row). Once the cursor is provably after the row's content, C-u is
//     well-defined regardless of where it started — the proxy stops
//     mattering because the precondition it was standing in for is now
//     actually true.
//  2. The no-movement latch. If an iteration — whichever shape it used —
//     produces NO movement (neither signal: text unchanged AND
//     composerVisualLines unchanged), the NEXT iteration uses the OTHER
//     shape (End+C-u <-> BSpace) rather than repeating the one that just
//     proved itself a no-op. A press that changes nothing is itself the
//     evidence the row-blankness guess was wrong for this row; alternating
//     costs one budget slot and converges without ever needing to observe
//     the cursor's column directly (the escalation this driver does not
//     have — see the ADR's Alternatives for why cursor-column reads via
//     `display-message` were rejected). A press that DOES move resets the
//     latch, so the ordinary blank-based choice governs again once
//     progress resumes.
//
// A composerClipped current row (muster#134: this driver could not
// read the row at all) is treated the same as non-blank — default to
// End+C-u and let the latch correct course if that guess is wrong, because
// "assume nothing is there to kill" is the direction that reproduces #134's
// own false negative one level down, inside the clear loop itself.
//
// composerLineEndKey is a named constant, not an inlined "End": if a
// substrate is ever measured NOT to bind End, the field fix is swapping
// this one constant to "C-e", not re-deriving the mechanism. Sending an
// unbound End is not free — an unrecognising TUI could echo its escape
// sequence as literal bytes, ADDING to the composer instead of leaving it
// alone — which is exactly what the no-movement latch bounds: one such
// press produces a capture that changed (worse, even) rather than one that
// stalled, and either way the loop does not repeat the same shape blindly.
//
// Verification stays in the loop: a keypress that did not register looks
// exactly like one that did, the same reason Send confirms before
// submitting.
//
// left is what remains (empty on success). moved reports whether the pass
// observed ANY movement at all — the caller's own signal for
// noteFutile/its own message, exactly as Discard used it before extraction.
// cleared is true only once the composer read back genuinely empty. err is
// non-nil only for a failed keystroke or capture call — "ran out of budget
// without emptying" is a normal outcome (cleared=false, err=nil), reported
// differently by each of the two callers.
func (d *Driver) clearComposer(ctx context.Context, paneID, id, cwd, pending string, expectedLines int, sc screen) (left string, moved, cleared bool, err error) {
	presses := expectedLines + clearPressMargin
	if presses < 1 {
		presses = 1
	}
	if presses > maxClearPresses {
		presses = maxClearPresses
	}
	left = pending
	curRows, _ := composerVisualLines(sc)
	curBlank, curBlankScan := composerCursorRowBlank(sc)
	stall := 0
	// altShape (muster#138): flips to true the moment a press produces
	// no movement, forcing the OPPOSITE key shape on the next iteration
	// instead of repeating the one that just proved itself a no-op — see
	// this function's own doc comment, "The no-movement latch". Reset to
	// false the moment a press DOES move something, so the ordinary
	// blank-based choice governs again once progress resumes.
	altShape := false
	for pressN := 0; pressN < presses; pressN++ {
		// #132/#138: a blank current row is a row C-u cannot make progress
		// against at all (#132) — cross it with Backspace instead. Anything
		// else — real content, OR a composerClipped row this driver could
		// not read (#134: assuming "nothing there" is the direction that
		// reproduces #134's own false negative one level down) — gets
		// positioned with composerLineEndKey before C-u, so C-u is
		// well-defined regardless of where the cursor actually started
		// (#138). altShape inverts whichever of the two this iteration
		// would otherwise have chosen, once a prior press has already
		// proven that choice a no-op.
		backspace := curBlank && curBlankScan == composerFound
		if altShape {
			backspace = !backspace
		}
		var keys []string
		if backspace {
			keys = []string{"BSpace"}
		} else {
			keys = []string{composerLineEndKey, "C-u"}
		}
		args := append([]string{"send-keys", "-t", paneID}, keys...)
		if _, runErr := d.run(ctx, d.bin, args...); runErr != nil {
			return left, moved, false, runErr
		}
		if next, ok := d.captureForClassify(ctx, paneID); ok {
			// composerFound is required alongside got=="" (muster#134):
			// a mid-pass capture that comes back composerClipped or
			// composerAbsent must not be read as "cleared" just because the
			// TEXT this driver could extract happens to be empty — both
			// composerText(next) return "" the same way a genuinely empty
			// composer does, and only composerFound actually LOOKED at the
			// whole composer to confirm that.
			got, gotScan := composerText(next)
			if got == "" && gotScan == composerFound {
				d.forgetFutile(id)
				return "", moved, true, nil
			}
			nextRows, _ := composerVisualLines(next)
			// A press counts as progress if it changed the joined TEXT (an
			// ordinary C-u killing real content) OR the composer's own row
			// count (a #132 Backspace merge collapsing a blank row away,
			// invisible to a text-only comparison — see this function's
			// doc comment for why).
			pressMoved := got != left || nextRows != curRows
			if pressMoved {
				moved = true
				stall = 0
				altShape = false
			} else {
				if moved {
					stall++
				}
				altShape = !altShape
			}
			left = got
			curRows = nextRows
			curBlank, curBlankScan = composerCursorRowBlank(next)
		}
		if moved && stall >= stallPresses {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if pressN == presses-1 {
			break
		}
		select {
		case <-ctx.Done():
			return left, moved, false, ctx.Err()
		case <-time.After(promptClearInterval):
		}
	}

	if !moved {
		// First time this exact residue has been seen not to move — record
		// it so a caller that retries against the SAME residue is refused
		// (futileClearAttempts) before spending another full pass on it.
		d.noteFutile(id, cwd, composerTextDigest(pending))
	}
	return left, moved, false, nil
}

// clearComposerSweep is muster#136's escape hatch: a character-
// budgeted Backspace sweep, reachable only through Discard's opts.Force
// once the ordinary row-budgeted pass (clearComposer, above) has already
// been proven futile against this EXACT residue (discardProvenFutile's own
// refusal is what names this as the next step).
//
// # Why Backspace alone, unconditionally, is the mechanism that reaches
// what the structural pass could not
//
// clearComposer chooses between C-u and Backspace based on the composer's
// STRUCTURE — which row the cursor is assumed to sit on, whether that row
// reads as blank (#132), whether positioning first makes C-u well-defined
// (#138). Every one of those readings can be wrong for the same reason
// #138 exists at all: an assumption about where the cursor actually is,
// not a direct observation of it. Backspace sidesteps the whole question —
// it deletes the ONE character behind the cursor unconditionally, crosses
// a newline exactly the way it crosses any other character (merging the
// row above into whatever position the cursor lands on next), and cannot
// escape the composer widget itself: a Backspace at the very start of a
// text input is a no-op, not a way to type into something else. Pressed
// enough times, it reaches whatever is there regardless of shape — the
// property clearComposer's structural choice was already trying to
// approximate at the row level, spent here in a coarser, blunter, and more
// exhaustive way that does not depend on getting the row/cursor reading
// right.
//
// pending is the CALLER-corroborated text — Discard's own digest check,
// unconditional whether Force is set or not (see Discard's own doc
// comment). This presses keys unconditionally and trusts that
// corroboration already happened; it does not repeat it.
//
// # Sizing — characters, not rows (muster#129's argument, one level finer)
//
// The budget is len([]rune(pending)) + sweepMargin backspaces, capped at
// maxSweepBackspaces: this mechanism spends one press per CHARACTER, not
// one per structural row the way clearComposer's C-u does, so the unit the
// budget is sized in has to match. Sent in batches of sweepBatchSize
// backspaces per send-keys call rather than one call per character — this
// driver already commits to one argv shape per verb
// (classifyCaptureArgs' own single-source-of-truth discipline for capture;
// applied here to keep this mechanism from growing a second, driftable
// shape) — and deliberately not `send-keys -N` (a repeat-count flag that is
// version-dependent behaviour this driver does not otherwise rely on).
//
// Verified the same way clearComposer verifies: a keypress that did not
// register looks exactly like one that did, so this re-captures and
// re-reads composerText after every batch rather than assuming the presses
// landed.
//
// left is what remains (empty on success). cleared is true only once the
// composer reads back genuinely empty — composerFound required alongside
// got=="", same #134 discipline clearComposer's own success branch already
// applies, for the identical reason: a mid-sweep capture that comes back
// composerClipped must never be misread as "cleared". err is non-nil only
// for a failed keystroke or capture call.
func (d *Driver) clearComposerSweep(ctx context.Context, paneID, id, pending string) (left string, cleared bool, err error) {
	left = pending
	remaining := len([]rune(pending)) + sweepMargin
	if remaining > maxSweepBackspaces {
		remaining = maxSweepBackspaces
	}
	if remaining < 1 {
		remaining = 1
	}
	if _, runErr := d.run(ctx, d.bin, "send-keys", "-t", paneID, composerLineEndKey); runErr != nil {
		return left, false, runErr
	}
	for remaining > 0 {
		if ctx.Err() != nil {
			return left, false, ctx.Err()
		}
		batch := remaining
		if batch > sweepBatchSize {
			batch = sweepBatchSize
		}
		args := []string{"send-keys", "-t", paneID}
		for i := 0; i < batch; i++ {
			args = append(args, "BSpace")
		}
		if _, runErr := d.run(ctx, d.bin, args...); runErr != nil {
			return left, false, runErr
		}
		remaining -= batch
		if next, ok := d.captureForClassify(ctx, paneID); ok {
			got, gotScan := composerText(next)
			if got == "" && gotScan == composerFound {
				d.forgetFutile(id)
				return "", true, nil
			}
			left = got
		}
		if remaining > 0 {
			select {
			case <-ctx.Done():
				return left, false, ctx.Err()
			case <-time.After(promptClearInterval):
			}
		}
	}
	return left, false, nil
}

// discardComposerClipped reports that this driver cannot corroborate — or
// safely claim as clear — a composer taller than its own capture window
// (muster#134): composerText returned composerClipped, not
// composerFound or composerAbsent, so Discard has no text to diff against
// expectDigest and no honest way to report "already clear". Claiming
// success here would be #134's own false negative arriving through Discard
// specifically: a caller retrying after a timeout, told "accepted", walking
// away from a composer that may still hold exactly the text it started
// with.
//
// Wrapped in ErrAmbiguousTarget for the same reason discardIncomplete and
// discardProvenFutile are (409, not 400): the request was well formed, and
// what failed is that this driver could not read enough of the screen to
// carry it out — not a caller mistake to fix by resending the same digest.
//
// This is deliberately a DIFFERENT message from both of those: it is not
// "the keystroke did not register" (nothing was pressed at all) and not
// "a full pass already proved this futile" (no pass has been attempted —
// clearComposer is never reached for this scan result). A caller that
// pattern-matches on either of those substrings to decide what to do next
// must see neither one here.
//
// It used to end "wait for the composer to shrink into the capture window …
// and retry". On an unattended session with a retrying caller that condition
// never arrives, so the advice sent callers into a loop that could not end
// (muster#149). The message now says what is actually true: retrying
// changes nothing, and the exit is outside this driver.
func discardComposerClipped(s screen) error {
	return fmt.Errorf(
		"%w: discard: this composer "+composerClippedCause(s)+", so "+
			"its content could not be read in full — neither \"already clear\" nor a "+
			"digest-corroborated clear is honest here. %s",
		ErrAmbiguousTarget, clippedComposerRemedy)
}

// clippedComposerRemedy is the one next-step sentence every clipped-composer
// refusal carries — discard, send and keys alike — so the three cannot drift
// into promising different ways out (muster#149).
//
// It deliberately names no API call as the fix. There is no request shape
// that proves the unseen rows hold nothing worth keeping (ADR 149 walks the
// candidates: a wider capture, a caller digest, provenance, a force flag),
// and closing the session is the disproportionate remedy #136 already
// retired for a stuck composer.
const clippedComposerRemedy = "Retrying does not change this: the same call, " +
	"with any expect or force, gets this same refusal while the composer stays " +
	"taller than the capture window, reaches above the visible pane (rows above " +
	"it are scrollback, muster#169) or is cut off by the pane's bottom edge " +
	"(muster#216), and nothing this driver can read proves " +
	"the unseen rows hold nothing worth keeping (muster#149). The way out " +
	"is a person reading or clearing the composer at the pane itself, or, for a " +
	"composer cut off at the bottom, a pane tall enough to draw it"

// discardIncomplete reports a clear that ran out of time without ever
// emptying the composer, and says which of two situations that is — because
// "still not empty" covers two outcomes a caller must treat oppositely, and
// nothing distinguished them before this existed.
//
// # The two outcomes, and why they cannot share a message
//
// Unchanged means the keystroke never registered at all: the composer reads
// exactly what the caller already corroborated (before, the digest-verified
// pending text). Nothing was destroyed, so retrying Discard with the same
// digest is exactly as safe as the first attempt was.
//
// Changed-but-nonempty means it registered PARTIALLY: some of the text is
// gone, none of it cleanly, and the composer now holds neither what the
// caller saw nor nothing — the worst of the three possible outcomes,
// because a caller told only "not cleared" cannot tell it apart from either
// of the other two. Retrying blind is actively dangerous here: the digest
// this failure leaves behind no longer matches what the caller read, so a
// retry against the old digest will be refused as stale (§5.4) anyway, and
// a caller that worked around that by re-reading and retrying without
// looking at what it now read could clear further into a corrupted message
// instead of stopping to ask a human.
//
// # Why both still map to conflict, not invalid
//
// Both are wrapped in ErrAmbiguousTarget, which the service maps to 409
// (conflict), not 400 (invalid) — see writeDriverError. The request was
// well formed; what failed is that the driver could not carry it out, which
// is not a caller mistake to fix by resending the same bytes. §5.4's kind
// already exists for "well-formed request, state the driver cannot
// corroborate" for the PRE-condition (the digest disagreeing before
// acting); this is the identical shape of problem at the POST-condition
// (the result disagreeing after acting), so it reuses the same kind rather
// than inventing a new one the wire's closed error-kind set does not have
// a slot for.
//
// # Why this is a message, not a field
//
// WaitingReason exists as a structured field because waiting_input started
// meaning two things that demand OPPOSITE automated handling — a prompt
// wants an answer, unsent input must not be sent to. This does not: in
// both cases here the correct caller action is identical — do not retry
// blind, re-read the composer before deciding anything — so a discriminator
// nothing would ever branch on would be dead weight. It is also not this
// file's convention: every other ErrAmbiguousTarget case in this driver
// (Close, Rename, and the two above) differentiates by message text alone,
// and a caller of this API already gets the general instruction for
// conflict — re-read and decide — from api-http.md §2.
//
// # #87: "unchanged" stopped meaning "safe AND worth retrying"
//
// This function only ever sees the FIRST time a given residue is observed
// not to move — Discard's futileClearAttempts check above refuses a repeat
// before this is even reached, so "safe to retry" here is no longer the
// lie it used to become on the second, third, fourth identical call. See
// discardProvenFutile for what a caller gets once retrying stops being
// useful, and this call's own doc comment for why the two must not share a
// message.
func discardIncomplete(before, after string) error {
	if after == before {
		return fmt.Errorf(
			"%w: discard: the clear keystroke did not register; the composer is "+
				"unchanged from what was read, so retrying once more with the same "+
				"digest is safe — if it is still unchanged after that retry, stop and "+
				"re-read rather than retrying again",
			ErrAmbiguousTarget)
	}
	return fmt.Errorf(
		"%w: discard: the clear keystroke ran but did not finish; the composer now "+
			"holds neither the original text nor nothing (found digest %s) — it is "+
			"damaged, not merely unclear, so re-read it before doing anything else "+
			"rather than retrying blind",
		ErrAmbiguousTarget, screenDigest(after))
}

// discardProvenFutile reports the state discardIncomplete's "unchanged"
// branch cannot: this is not the first pass against this residue, it is at
// least the second, and the prior pass already spent a full, content-sized
// press budget (clearComposer's expectedLines+clearPressMargin, capped at
// maxClearPresses — muster#129) pressing its clear keys (C-u, and
// Backspace wherever #132 finds a blank row) against it without moving it
// at all (#87).
//
// The Issue this closes described exactly the failure this guards against:
// a caller that followed "retrying with the same digest is safe" to the
// letter, four times, and made zero progress each time — the advice was
// true in the narrow sense (nothing was destroyed) and false in the sense
// that mattered (retrying was never going to help). This message stops
// making that promise once the evidence for it is gone, and — unlike the
// "safe to retry" wording — deliberately contains neither "safe" nor
// "unchanged", so a caller pattern-matching on either of those substrings
// to decide whether to retry sees a different answer here, not a repeat of
// the first one.
//
// # muster#136: this used to dead-end at destroying the session
//
// `keys` refuses outright while the composer holds text (see keys.go), and
// this file's own history already measured Escape as not helping here
// (Discard's C-u comment: "C-a C-k and Escape were tried too") — rejected
// again, explicitly, as #136's own remedy: opening `keys` to Escape here
// would hand a caller a door this driver has already measured not to open.
// What this message used to say was left, and what it said, was the
// session-level operation guaranteed to work regardless of what state the
// composer is stuck in: close it. That was disproportionate — a session
// carries a conversation, a bridge, in-flight work, and for a caller that
// binds them, a claim and a worktree, none of which respawning recovers —
// and enshrining it as the documented next step made an admission of a gap
// read as the ordinary remedy.
//
// It now names `?force=true` instead: Discard's own opts.Force reaches for
// clearComposerSweep, a character-budgeted Backspace sweep past whatever
// shape defeated the structural pass this message is reporting on. Still
// corroborated — expectDigest is unconditional whether Force is set or
// not — so this is not a relaxation of §5.4, only a stronger mechanism
// available once the ordinary one has already been proven not to work
// against this exact residue.
func discardProvenFutile(attempts int) error {
	return fmt.Errorf(
		"%w: discard: a full clear pass already left this exact composer text "+
			"unmoved %d time(s); pressing again is not expected to do anything "+
			"different, so this call made no attempt — re-read the composer before "+
			"deciding anything. A stronger clear is available: retry with "+
			"?expect=<the same digest>&force=true to sweep the composer character by "+
			"character rather than by structural keystroke; closing the session "+
			"(DELETE /v1/machines/{machine}/sessions/{id}) should not be needed for a "+
			"stuck composer alone",
		ErrAmbiguousTarget, attempts)
}

// Rename changes a session's id (§3).
//
// # The id IS the name on this substrate
//
// A multiplexer session's name is its handle: it is what an operator sees in
// their status bar, what a picker lists, and what every command targets. So a
// rename here is not cosmetic relabelling — it changes the very thing callers
// address the session by, which is why the service emits an event and why this
// corroborates before acting.
//
// # Corroborated exactly like Close, and for a sharper reason
//
// Close destroys the wrong session if the id was recycled. Rename does
// something subtler and arguably worse: it succeeds, silently, on a session
// the caller never meant — and leaves BOTH sessions misnamed, the target
// wearing a name that belongs to another piece of work. §5.4's rule is the
// same and the failure is quieter, so the check is the same.
//
// # Returns RenameAck, not Ack — and never sets its own Title
//
// This method reports only the id half (muster #222): whether the
// multiplexer-level rename happened. RenameAck.Title is always left nil
// here; the service calls SyncTitle (titlesync.go) separately, AFTER this
// returns and after it has announced session.renamed, and fills Title in
// itself (internal/service/rename_title.go). Splitting it this way keeps
// the id-change announcement exactly as timely as it was before this
// existed — nothing here waits on a composer delivery or a transcript poll.
func (d *Driver) Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.RenameAck, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	to = strings.TrimSpace(to)
	if to == "" {
		return fleet.RenameAck{}, errors.New("rename: new name is empty")
	}
	if to == ref.ID {
		// Not an error: the caller asked for a state that already holds.
		return fleet.RenameAck{Accepted: true}, nil
	}

	// muster #223: refuse a name the multiplexer would silently mangle
	// (naming.go's sanitizeName — e.g. '.' -> '-', a leading '-' dropped),
	// rather than rename to it and announce an id that is not the
	// multiplexer's real one. This is the same principle as the collision
	// check just below — refuse rather than let the multiplexer decide —
	// applied to shape instead of uniqueness.
	//
	// Deliberately a REFUSAL, not Create's silent clean-and-proceed
	// (resolveName): a create's requested name was never authoritative
	// anyway (it may still be renumbered on collision), but a rename's `to`
	// is client-facing top to bottom — session.renamed announces it
	// verbatim, and RenameAck carries no "resolved name" field a caller
	// could read instead. Silently renaming to a DIFFERENT string than what
	// was asked for would just move today's mismatch from "this driver vs.
	// the multiplexer" to "this driver vs. its own caller". Surfacing the
	// clean form in the error lets the caller retry with a name this
	// substrate can hold exactly.
	if clean := sanitizeName(to); clean != to {
		if clean == "" {
			return fleet.RenameAck{}, fmt.Errorf("rename: %q sanitizes to nothing this multiplexer can hold as a session name", to)
		}
		return fleet.RenameAck{}, fmt.Errorf("rename: %q is not a name this multiplexer can hold as given; retry with %q", to, clean)
	}

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.RenameAck{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
		}
		// Refuse a collision rather than letting the multiplexer decide. Two
		// sessions cannot share a name, and the failure mode of finding out
		// afterwards is an operator who believes a rename happened.
		if rows[i].session == to {
			return fleet.RenameAck{}, fmt.Errorf("rename: %q is already in use on this machine", to)
		}
	}
	if live == nil {
		return fleet.RenameAck{}, d.noSuchSession(ctx, rows, ref.ID)
	}

	if want := req.Expect.StartedAt; want != nil {
		if !live.created.Equal(*want) {
			return fleet.RenameAck{}, fmt.Errorf(
				"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
				ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	} else {
		// The weak guarantee, named as weak — same shape as Close.
		d.mu.Lock()
		prior, seen := d.observed[ref.ID]
		d.mu.Unlock()
		if !seen {
			return fleet.RenameAck{}, fmt.Errorf(
				"%w: caller supplied no expected start time, and this driver has no prior "+
					"observation of id %q either; nothing corroborates the target",
				ErrAmbiguousTarget, ref.ID)
		}
		if !live.created.Equal(prior.created) || live.cwd != prior.cwd {
			return fleet.RenameAck{}, fmt.Errorf(
				"%w: id %q was recycled since this driver last observed it (weak check)",
				ErrAmbiguousTarget, ref.ID)
		}
	}

	// muster #222: alias the composer-serialisation lock BEFORE the
	// multiplexer rename runs, so a title-sync delivery this service is
	// about to make against the NEW id (SyncTitle, moments from now) shares
	// the identical mutex with anything already in flight against the OLD
	// id — both address the same underlying pane, and a rename in between
	// must not let them hold two different locks over it (terminalpath2_lock.go's
	// own D5 hazard, reopened by rename). Rename itself never waits on this
	// lock: a busy composer must not turn a rename into a failure.
	d.aliasComposerLock(ref.ID, to)

	// muster #222: prime the conversation memo under the OLD name,
	// before it stops matching anything. Name-and-date derivation
	// (conversation.go) matches the transcript whose FIRST custom-title is
	// still ref.ID at this exact moment; the memo it populates is keyed on
	// (pane, created), which survives the rename below untouched. Without
	// this, a title-sync lookup moments from now — asking with the NEW
	// name, which the transcript does not carry as a title anywhere yet —
	// would have nothing to derive from until the runtime writes a fresh
	// record under the new name, if it ever does.
	if d.conversations != nil {
		d.conversations.lookup(conversationKey{pane: live.paneID, created: live.created}, live.cwd, ref.ID,
			live.created, processGeneration{pid: live.pid}, d.liveConversationSource(ctx, live.pid, live.cwd))
	}

	// "=" pins an exact name. Without it the multiplexer resolves prefixes and
	// patterns, and would happily rename a DIFFERENT session whose name merely
	// starts with this one — which on a fleet full of `<repo>-<issue>` names is
	// not a hypothetical.
	if _, err := d.run(ctx, d.bin, "rename-session", "-t", "="+ref.ID, to); err != nil {
		return fleet.RenameAck{}, fmt.Errorf("rename: %w", err)
	}

	// Carry the driver's own memory across, or the renamed session looks
	// brand new: its `since` would reset and §12 would call it adopted.
	d.mu.Lock()
	if prior, ok := d.observed[ref.ID]; ok {
		d.observed[to] = prior
		delete(d.observed, ref.ID)
	}
	d.mu.Unlock()

	// muster #97: write the durable half. The multiplexer call above
	// just demonstrated the rename reaches the runtime — that has never
	// been in question — but nothing until now recorded that this driver
	// EXPECTS the session to be named `to`, so nothing has ever put it back
	// if a second actor on the machine undoes it later. This is what
	// List's identityDrift/reassertNames read.
	d.noteRenamed(ref.ID, to, live.cwd, live.paneID, live.created)
	// #185: a delivery lane belongs to the process, not the name.
	d.mods.rekey(ref.ID, to)
	// #223: the four per-id records #185/#97/d.observed's own move above did
	// not already cover — see rekeySessionState's own doc comment for why
	// each of the four needs a rename move at all.
	d.rekeySessionState(ref.ID, to)

	return fleet.RenameAck{Accepted: true}, nil
}

func (d *Driver) killCorroborated(ctx context.Context, ref fleet.SessionRef) (fleet.Ack, error) {
	if _, err := d.run(ctx, d.bin, "kill-session", "-t", ref.ID); err != nil {
		return fleet.Ack{}, fmt.Errorf("close: %w", err)
	}
	d.mu.Lock()
	delete(d.observed, ref.ID)
	d.mu.Unlock()
	// #11: a destroyed session's composer is gone with it, so any stranded
	// record for this id has nothing left to resume into. Forgetting it here
	// is the proactive half — strandedMatches's cwd check and
	// strandedRetention are the backstop for every path that isn't an
	// explicit Close, but there is no reason to leave this one waiting out
	// its window when Close already knows it is dead.
	d.forgetStranded(ref.ID)
	// #111: a destroyed session's `turns` denominator describes a delivery
	// into a composer that no longer exists — forget it the same way and
	// for the same reason as the stranded record just above.
	d.forgetDelivery(ref.ID)
	// #185: tell the session's delivery module its lane is finished. The kill
	// above is the authority; a failure here never blocks it and is retried
	// when the module is next ready.
	d.mods.closeLane(ref.ID)
	return fleet.Ack{Accepted: true}, nil
}

// reapDeadRows finds, among an ALREADY-fetched enumeration, every pane whose
// own process has already exited AND whose session this driver's own Create
// marked as its own (managedSessionOption) — remain-on-exit (see Create's own
// comment on the option) is the only reason such a row can exist in rows at
// all, and nothing about a pane going dead reaches control mode's lifecycle
// notifications the way a session or window actually closing does
// (subscribe.go's isLifecycleNote), so this is where "something looks".
//
// The marker check is load-bearing, not defensive: Create's own comment on
// remain-on-exit says this driver's multiplexer server may host sessions
// other tools started, and muster #236 found this func acting on every
// dead pane it saw regardless of origin — an operator (or another tool) that
// sets remain-on-exit on its own session, for its own reasons, had this
// driver kill it, capture its screen, and report an exit it never owned, the
// moment that session's process happened to exit. A dead pane with no marker
// is left exactly as it was before muster #235 ever shipped: not killed,
// not captured, not reported — this func has no opinion about it at all.
//
// The screen this SAME enumeration already captured for it (classifyCaptureArgs'
// shape — the escapes are harmless in a plain evidence file, and reusing it
// is what keeps this from being a second, hand-rolled capture-pane call; see
// TestClassifierCapturesAreBuiltInOnePlace) is what gets saved as its last
// screen. Then the session is killed through the same path an ordinary Close
// uses, so nothing about its bookkeeping is special-cased, and it is dropped
// from what List returns — a session this driver just confirmed dead must
// never be reported live.
//
// Ordinary case (nothing dead-and-managed) costs nothing extra at all: no new
// invocation, matching driver.Driver.List's constant-spawn contract. Killing
// a genuinely dead session is the one case that is NOT constant-cost, and it
// is meant to be — it is rare by construction and the alternative is a
// session lingering forever under remain-on-exit.
//
// Best-effort: a kill that fails leaves the row exactly where it was for the
// next List to find and retry — nothing is reported until the kill lands, so
// a retry can never double-report the same exit.
func (d *Driver) reapDeadRows(ctx context.Context, rows []paneRow, captures map[string]paneCapture) []paneRow {
	hasReapable := false
	for _, r := range rows {
		if r.dead && r.managed {
			hasReapable = true
			break
		}
	}
	if !hasReapable {
		return rows
	}
	kept := make([]paneRow, 0, len(rows))
	for _, r := range rows {
		if !r.dead || !r.managed {
			kept = append(kept, r)
			continue
		}
		at := d.now()
		screenPath := ""
		if c, ok := captures[r.paneID]; ok {
			screenPath = d.saveExitScreen(r.session, c)
		}
		if _, err := d.killCorroborated(ctx, fleet.SessionRef{Machine: d.machine, ID: r.session, Name: r.session}); err != nil {
			// Still there for the next List to find and try again.
			kept = append(kept, r)
			continue
		}
		d.notePendingExit(r.session, fleet.SessionExit{Status: r.deadStatus, At: at, ScreenPath: screenPath})
	}
	return kept
}

// saveExitScreen writes a dying pane's last captured screen to a capped,
// private file in this driver's own state directory (muster #235).
// Never served over HTTP — pane text can hold anything the runtime printed,
// and putting it on the API is a new data-exposure surface the issue's
// ruling deliberately did not open; only the returned path goes on the
// history record.
//
// Best-effort: an unconfigured store, or any failure here, returns "" rather
// than failing the reap that depends on it — an exit with no screen is still
// worth recording with its status alone.
func (d *Driver) saveExitScreen(session string, c paneCapture) string {
	if d.store == nil {
		return ""
	}
	dir := filepath.Join(d.store.Dir(), "exit-screens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	path := filepath.Join(dir, session+"-"+d.nonce()+".txt")
	if err := os.WriteFile(path, []byte(c.text), 0o600); err != nil {
		return ""
	}
	return path
}

// notePendingExit queues a captured exit for the next DrainExits (muster
// #235). Its own mutex, not d.mu — see exitedMu's doc comment on the Driver
// struct.
func (d *Driver) notePendingExit(id string, e fleet.SessionExit) {
	d.exitedMu.Lock()
	d.exitedPending = append(d.exitedPending, driver.CapturedExit{ID: id, Exit: e})
	d.exitedMu.Unlock()
}

// DrainExits implements driver.ExitReporter (muster #235): every exit
// captured since the last drain, forgotten the moment it is returned so
// nothing is ever reported twice.
func (d *Driver) DrainExits() []driver.CapturedExit {
	d.exitedMu.Lock()
	defer d.exitedMu.Unlock()
	if len(d.exitedPending) == 0 {
		return nil
	}
	out := d.exitedPending
	d.exitedPending = nil
	return out
}

// Create starts a session (§3), honouring the caller's idempotency key
// (§10).
//
// The caller's prompt never reaches a command line. §5.3 requires context
// to travel as a path, and the same reasoning applies to the prompt itself:
// process command lines are a shared namespace, and anything that matches
// processes by name can match — and terminate — a session whose argv merely
// contains the string it was hunting for. The prompt is written to a file
// and delivered through the paste buffer once the session is up.
//
// # This never refuses because the account is refusing work (#10)
//
// Deliberately absent: a check against quotaBlock before starting the
// multiplexer session. A create must not be silently refused because the
// account behind the machine is refusing work — the caller is told and
// decides, joining `discard`-without-digest or `respond`-without-prompt
// would report as a well-formed create that "cannot succeed", when in fact
// the session it names is perfectly real and will exist to try again the
// moment the account does not refuse.
//
// The report itself does not happen here, and does not need to: the caller
// learns it from the state this call's own SessionRef reads back as, which
// is quotaBlockedState applied to whatever State (or a subsequent List)
// returns for it. That is deliberate, not an oversight — a session created
// while blocked starts out `starting`, which quotaBlockedState treats as
// silence about the account rather than evidence against it, the same as
// `idle` and `unknown` already were. See quotaBlockedState for the shared
// rule and Driver.State for where it is applied to the read Create's own
// HTTP handler makes to build a 201 response.
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.Session{}, errors.New("create: idempotency key is required (§10)")
	}
	// muster #280: this driver starts a session inside a login shell that
	// inherits the service's environment, so it has no honest way to deliver a
	// BUILT one. A create that requires isolation is refused as unsupported —
	// before any side effect — rather than started with the service's whole
	// environment (§2.1: refuse rather than drop a hint silently).
	if spec.IsolateEnvironment {
		return fleet.Session{}, &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: "create: this driver cannot isolate a session's environment (it starts sessions in a shell " +
				"that inherits the service's own); refusing rather than start a session that leaks it",
			Machine: d.machine,
		}
	}
	// muster #281: this driver starts sessions through the user's multiplexer,
	// whose server socket is the very thing a sandbox must keep a session away
	// from, so it cannot confine one. Refused before any side effect.
	if spec.Sandbox != nil {
		return fleet.Session{}, &fleet.Error{
			Kind: fleet.ErrorUnsupported,
			Message: "create: this driver cannot enforce a sandbox (its sessions run in the user's own multiplexer, " +
				"outside any profile this service could apply); refusing rather than start the session unconfined",
			Machine: d.machine,
		}
	}
	// A completed key returns what it produced; a pending one means this
	// driver was interrupted mid-create and must find out what happened
	// before doing anything (§10, see idempotency.go).
	if ref, rec, found := d.idem.lookup(key); found {
		if rec.Phase == idemComplete {
			// muster #234: a completed key's session can have ended since
			// it was recorded — the process exited, someone killed it from a
			// terminal — and returning it here unconditionally answered with
			// the same 201-shaped session a live create gets, with nothing to
			// tell the two apart. Corroborate against what is actually running
			// before replaying the record, the same existence check
			// resolvePending below makes for a PENDING record's own recovery.
			live, err := d.sessionLive(ctx, ref.ID)
			if err != nil {
				// §5.7: a failed read is never presented as an observation —
				// this driver does not know whether the session survives, so
				// it must not answer as if it had checked. The idempotency
				// record is left exactly as it was; the caller's retry (of
				// this same call) gets another chance once the multiplexer
				// answers again.
				return fleet.Session{}, fmt.Errorf(
					"create: verifying that idempotency key %q's recorded session %q is still live: %w",
					key, ref.ID, err)
			}
			if !live {
				closedAt := d.now()
				return fleet.Session{}, &fleet.Error{
					Kind:     fleet.ErrorConflict,
					Reason:   fleet.ReasonReplayOfEndedSession,
					Message:  fmt.Sprintf("create: idempotency key %q already produced session %q, which is no longer present; mint a fresh key to start a new one (§10)", key, ref.ID),
					Machine:  d.machine,
					Session:  &fleet.SessionRef{Machine: ref.Machine, ID: ref.ID, Name: ref.Name},
					ClosedAt: &closedAt,
				}
			}
			cr, ok := d.createRecordFor(ref.ID, string(spec.Cwd))
			pins, surface, prompt := sessionFactsFor(cr, ok, ref.ID)
			return fleet.Session{
				SessionRef: ref, Cwd: spec.Cwd,
				Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
				Conversation:      conversationCapturedRef(spec.ConversationId),
				IdentityAssertion: d.identityAssertionForCreate(ref.ID),
				Marker:            d.markerForCreate(ref.ID),
			}, nil
		}
		if adopted, ok := d.resolvePending(ctx, key, rec); ok {
			cr, found := d.createRecordFor(adopted.ID, string(spec.Cwd))
			pins, surface, prompt := sessionFactsFor(cr, found, adopted.ID)
			return fleet.Session{
				SessionRef: adopted, Cwd: spec.Cwd,
				Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
				Conversation:      conversationCapturedRef(spec.ConversationId),
				IdentityAssertion: d.identityAssertionForCreate(adopted.ID),
				Marker:            d.markerForCreate(adopted.ID),
			}, nil
		}
		// Nothing was started, or nothing survives. Safe to proceed.
		_ = d.idem.release(key)
	}

	// The name is resolved BEFORE the argv is built, and this ordering is the
	// seam the whole creation contract hangs on.
	//
	// The resolved string is what the multiplexer session is called, what the
	// remote-control binding is keyed on, and what the agent calls itself. A
	// builder handed the REQUESTED name would bind remote control to a name
	// the session does not have — which fails exactly the way this whole area
	// fails: silently, later, and somewhere else.
	requested := spec.Name
	if requested == "" {
		requested = "fleet-" + d.nonce()
	}
	name, markerApplied, ok := d.resolveName(ctx, requested, spec.Marker)
	if !ok {
		return fleet.Session{}, fmt.Errorf(
			"create: could not derive a free session name from %q; either it sanitizes "+
				"to nothing, or too many sessions already carry it", requested)
	}
	if spec.Cwd == "" {
		return fleet.Session{}, errors.New("create: cwd is required")
	}

	contextFile := string(spec.ContextRef)
	if contextFile != "" && !filepath.IsAbs(contextFile) {
		return fleet.Session{}, fmt.Errorf("create: contextRef must be absolute, got %q", contextFile)
	}
	// muster issue #94: fold this machine's declared identity into the
	// caller's env BEFORE any of the validation below, so a configured value
	// is checked by the same bound as a caller's own (see sessionenv.go's
	// readSessionEnvFile) and a bareExec driver refuses a configured value
	// exactly as it already refuses a caller-supplied one. Must run here,
	// inside the local driver's own Create — never in the HTTP handler — so
	// a create this machine only relays onward never reads this machine's
	// files (see sessionenv.go's package doc for why).
	// #180: a caller may not set a name the delivery module reserves — the
	// service is the sole setter of those. Checked against the CALLER's env,
	// before this machine's own sessionEnv is merged in (ValidateSessionEnv
	// already refused a configuration naming one at startup).
	if err := delivery.CheckReservedEnv(spec.Env, d.ReservedEnv()); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	// #185: and by prefix — a module declares those in its own handshake, and
	// the guard holds whether or not it is installed or running right now.
	if err := delivery.CheckReservedEnvPrefixes(spec.Env, d.ReservedEnvPrefixes()); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	mergedEnv, err := d.provisionSessionEnv(spec)
	if err != nil {
		return fleet.Session{}, err
	}
	spec.Env = mergedEnv
	if err := validateEnv(spec.Env); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	// Refuse rather than start a session missing what the caller asked for. The
	// bare-exec shape has no shell to apply an environment file in, so a create
	// carrying variables cannot be honoured there — and a session that comes up
	// without the identity its supervisor gave it looks perfectly healthy and
	// fails later, somewhere else, which is the failure mode this whole area
	// keeps producing. This also catches a value sessionEnv contributed above:
	// bareExec has no out-of-band channel for it either.
	if len(spec.Env) > 0 && d.bareExec {
		return fleet.Session{}, errors.New(
			"create: this driver is configured without the login-shell wrap, so it has " +
				"no out-of-band channel for env; refusing rather than starting a session without it")
	}
	if spec.PermissionMode != "" && spec.PermissionMode != fleet.PermissionModeBypass {
		return fleet.Session{}, fmt.Errorf(
			"create: unknown permissionMode %q (this runtime has one: %q)",
			spec.PermissionMode, fleet.PermissionModeBypass)
	}
	if err := validateMcpConfig(spec.McpConfig); err != nil {
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	// muster #247/#254: launch-time settings must be a JSON object, and outside
	// bypass may carry only the allow-listed keys. Checked here as well as in the HTTP handler — Create is a public
	// method and must not trust that another layer already validated.
	if _, err := fleet.ValidateLaunchSettings(spec.PermissionMode, spec.Settings); err != nil {
		return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
	}
	if spec.Resume != "" && !safeArgvValue(spec.Resume) {
		return fleet.Session{}, fmt.Errorf(
			"create: resume %q would be read as a flag by the agent, not as a conversation id",
			spec.Resume)
	}
	// muster #224: conversationId asks for a NEW conversation, resume
	// asks to continue one — sending both answers the same question two
	// incompatible ways. Checked again here even though handleCreateSession
	// already refuses this combination before any driver is reached (§10 of
	// this repo's own docs/api.md): this method is a public Driver method a
	// test or another embedder may call directly, and it must not trust that
	// some other layer already validated its own argument.
	if spec.ConversationId != "" {
		if spec.Resume != "" {
			return fleet.Session{}, &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: "create: conversationId and resume are mutually exclusive — one starts a " +
					"new conversation, the other continues one",
				Machine: d.machine,
			}
		}
		if err := fleet.ValidateConversationId(spec.ConversationId); err != nil {
			return fleet.Session{}, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "create: " + err.Error(), Machine: d.machine}
		}
		// "Already known as a live conversation" (the Ask in #224): a record
		// already on file for this id, in this working directory's own store,
		// is not proof a session is running against it RIGHT NOW, but it is
		// proof that starting a fresh conversation under the same id would
		// make two conversations share one transcript file — which #224 asks
		// to refuse regardless of whether the earlier session is still alive.
		// Scoped to spec.Cwd deliberately: the runtime's own store partitions
		// records by working directory (recordDirFor), so an id colliding
		// under a DIFFERENT cwd is not a collision the runtime could ever
		// produce here. This is a stat, not a lock — a second create for the
		// same id racing this one between the check and the launch below is
		// not closed by it, the same TOCTOU every other pre-launch check in
		// this function already lives with.
		if d.conversations != nil {
			if path, ok := d.conversations.recordPathWithin(string(spec.Cwd), spec.ConversationId); ok {
				if _, statErr := os.Stat(path); statErr == nil {
					return fleet.Session{}, &fleet.Error{
						Kind: fleet.ErrorInvalid,
						Message: fmt.Sprintf("create: conversationId %q already names a conversation "+
							"recorded for this working directory; two sessions must never share one transcript",
							spec.ConversationId),
						Machine: d.machine,
					}
				}
			}
		}
	}
	// muster #84: Agent/Model/Effort get the same guard Resume already has,
	// four lines above. Before this, a value beginning with "-" silently failed
	// safeArgvValue inside claudeCodeCommand, the flag was never appended, and
	// the create response still echoed the REQUESTED value back — telling a
	// caller a pin was applied when the session was running whatever the
	// runtime defaults to. Refusing here, before any argv is built, is §2.1's
	// own rule: "a driver that cannot honour [a hint] must say so at creation
	// ... rather than silently substituting a default."
	for _, pin := range []struct{ name, value string }{
		{"agent", string(spec.Agent)},
		{"model", spec.Model},
		{"effort", spec.Effort},
	} {
		if pin.value != "" && !safeArgvValue(pin.value) {
			return fleet.Session{}, &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: fmt.Sprintf(
					"create: %s %q would be read as a flag by the agent, not as a value; "+
						"refusing rather than starting a session with the pin silently dropped (§2.1)",
					pin.name, pin.value),
				Machine: d.machine,
			}
		}
	}
	for _, k := range spec.Consents {
		if _, ok := consentableKinds[k]; !ok {
			return fleet.Session{}, fmt.Errorf(
				"create: %q is not a consentable question — see the driver's note on "+
					"why some boot questions have no safe affirmative option", k)
		}
	}

	// Intent first. A crash between here and the session starting leaves a
	// pending record, which the next attempt resolves by looking rather
	// than guessing.
	if err := d.idem.reserve(key, name, string(spec.Cwd)); err != nil {
		return fleet.Session{}, fmt.Errorf("create: recording intent: %w", err)
	}

	// The builder sees the RESOLVED name, not the requested one — see above.
	built := spec
	built.Name = name
	argv := d.build(built, contextFile)

	// Wrap the agent in a login+interactive shell so it inherits the same
	// environment a launcher-created session does, and stage a record of what
	// it actually ended up with. See environment.go for why interactive is not
	// optional and why the record carries no values.
	//
	// #151: the wrapper also enters spec.Cwd itself. `-c` below is kept — it is
	// still what the multiplexer reports as the pane's path — but it is not
	// trusted to be what the agent starts in. A bare-exec session has no shell
	// to do the `cd`, so it stays exposed to a server with a dead directory;
	// that is part of what opting out of the wrapper costs.
	//
	// #185: an enabled delivery module may add environment of its own here —
	// the ONLY way a name it reserves gets set. It can only add a lane, never
	// stop a create: on any refusal the session simply launches without one.
	launchEnv, _ := d.mods.offerLane(ctx, name, string(spec.Cwd), spec.Env, !d.bareExec)
	envPath, err := d.stageEnv(launchEnv)
	if err != nil {
		d.mods.abandon(name)
		_ = d.idem.release(key)
		return fleet.Session{}, fmt.Errorf("create: staging env: %w", err)
	}
	recordPath := ""
	if !d.bareExec {
		recordPath = d.envRecordPath()
		argv = loginWrap(d.loginShell(), recordPath, envPath, string(spec.Cwd), argv)
	}

	// #47, point 5: seed this session's own working directory before the
	// process that would ask about it is even started, closing the same
	// race a periodic-only pass leaves open for a worktree younger than the
	// interval. Best-effort by design — a refusal, a lost race, or the
	// feature being unconfigured must never fail a create it is only trying
	// to help; d.trustSeed is nil-safe (see WithTrustSeed) and the error, if
	// any, is already counted inside it. This session simply meets its
	// Consents path below, exactly as it would have without this.
	if err := d.trustSeed.SeedPath(string(spec.Cwd)); err != nil {
		log.Printf("tmux: trust-seed: %v", err)
	}

	args := append([]string{
		"new-session", "-d", "-s", name, "-c", string(spec.Cwd), "--",
	}, argv...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		// The create demonstrably failed, so the reservation describes
		// nothing. Releasing it keeps a retry from being answered with a
		// session that was never started.
		d.mods.abandon(name)
		_ = d.idem.release(key)
		// And the staged file is now certain to have no reader. It may hold a
		// credential, so it goes now rather than at the sweep below.
		if envPath != "" {
			_ = os.Remove(envPath)
		}
		return fleet.Session{}, fmt.Errorf("create: %w", err)
	}
	if envPath != "" {
		// The wrapper unlinks it the moment it has read it. This is the case
		// where it never does — the shell died, the agent binary was missing —
		// and a file of values must not outlive the session it was staged for.
		go d.sweepStagedEnv(envPath)
	}
	// muster #235: keep this session's pane around after its own process
	// exits, so reapExited (below, called from List) can capture why before
	// removing it. Without this, a pane and whatever it could have said about
	// its own exit vanish together the instant the process dies, and the
	// service can only ever record `closedBy: absent`. remain-on-exit is a
	// WINDOW option — targeting the session by name here scopes it to that
	// session's own (only) window, never global: this driver's multiplexer
	// server may host other tools' sessions too, and -g would silently change
	// their behaviour as well. Best-effort: a failure here does not fail a
	// create that already succeeded; the session simply falls back to
	// today's behaviour (vanishes with its process) if it never applies.
	if _, err := d.run(ctx, d.bin, "set-option", "-t", name, "remain-on-exit", "on"); err != nil {
		log.Printf("tmux: create %q: could not enable remain-on-exit, its own exit will not be captured: %v", name, err)
	}
	// muster #236: mark this session as one this driver started, in a
	// SEPARATE invocation for the same reason remain-on-exit above is
	// separate from new-session — chaining either into a single call would
	// fail the whole create over a rejected option, which is worse than the
	// residual race narrowed rather than closed here. reapDeadRows reads
	// this back (managedSessionOption) before ever killing a dead pane: a
	// session sharing this multiplexer server that this driver did not
	// start must never carry it, so a session created outside this driver
	// is left alone even with remain-on-exit set by someone else. A session
	// option, not -g and not -p: it is inherited by the session's own
	// panes without touching anything else on the server. Best-effort, like
	// remain-on-exit: a failure here does not fail a create that already
	// succeeded, and the session simply falls back to today's un-owned
	// behaviour (its dead pane, if any, is never reaped by this driver).
	if _, err := d.run(ctx, d.bin, "set-option", "-t", name, managedSessionOption, "1"); err != nil {
		log.Printf("tmux: create %q: could not mark it as managed, its dead pane will never be reaped: %v", name, err)
	}
	// muster #269: record whether the remote-control flag really went on the
	// command line — not what the caller asked for, which a missing name can
	// silently turn into "no flag" (claudeCodeCommand). `off` is only ever
	// claimed from this fact, never from the request. Best-effort: without it
	// the session simply makes no launch-based claim.
	launchRC := "0"
	for _, a := range argv {
		if a == "--remote-control" {
			launchRC = "1"
			break
		}
	}
	if _, err := d.run(ctx, d.bin, "set-option", "-t", name, launchRemoteControlOption, launchRC); err != nil {
		log.Printf("tmux: create %q: could not record its remote-control launch, `off` will not be claimed for it: %v", name, err)
	}
	// #185: the process exists now, so the module can be asked to attach —
	// asynchronously; this create's response never waits on it.
	d.mods.launched(name)

	ref := fleet.SessionRef{Machine: d.machine, ID: name, Name: name}
	if err := d.idem.complete(key, ref); err != nil {
		return fleet.Session{}, fmt.Errorf("create: recording result: %w", err)
	}
	if spec.Resume != "" {
		// #72: recorded now, before there is any way yet to tell whether
		// the runtime actually honoured it — see resumeintent.go.
		d.noteResumeIntent(name, string(spec.Cwd), string(spec.Resume))
	}
	if spec.ConversationId != "" {
		// #224: recorded now, for the mirror reason resumeIntent is recorded
		// above — before there is any way yet to tell whether the runtime's
		// own record agrees with the id this create asked it to start under.
		// See conversationintent.go.
		d.noteConversationIntent(name, string(spec.Cwd), spec.ConversationId)
	}
	// #84/#85/#86: recorded now, before there is any way yet to tell what
	// became of the pin, the surface, or the prompt — see createrecord.go.
	// The same record List reads back on every later listing, so the 201
	// body built below and the first 200 body are computed from one fact,
	// never two that can drift apart.
	d.noteCreateRecord(name, string(spec.Cwd), spec)
	// muster #96/#97: recorded now too, for the same "before there is
	// any way to tell what became of it" reason as the create record just
	// above — the marker fact #96 needs, and the identity #97's List/
	// reassertNames will have something to put back if the runtime ever
	// disagrees with it. Cwd/Pane/Created are filled in by noteSessionSet
	// on this session's first List (see its own "stub" branch); this call
	// only knows the name and the marker decision.
	d.noteAssertedName(name, string(spec.Cwd), name, sanitizeName(spec.Marker), markerApplied)

	if recordPath != "" {
		go d.captureEnvironment(name, recordPath)
	}
	// The step that answers a boot question runs whenever there is something for
	// it to do: work to deliver once the session is ready, or a consent to spend
	// on a question standing in front of it. A create carrying ONLY a consent —
	// no prompt, no trustCwd — used to skip it, so its 201 came back and the
	// question stayed on screen (muster #211). It returns as soon as the
	// composer is ready, so a consent with no question to meet costs one poll.
	if spec.Prompt != "" || spec.TrustCwd || len(spec.Consents) > 0 {
		go d.settleNewSession(req, ref, built)
	}
	rec, found := d.createRecordFor(name, string(spec.Cwd))
	pins, surface, prompt := sessionFactsFor(rec, found, name)
	// muster #102: the identity this call just asserted, read back from
	// the same durable record noteAssertedName just wrote above — honestly
	// unresolved, since nothing has read this session back yet and this
	// cannot claim the runtime carries it (that claim is List's to make, on
	// a later read). Same "one fact, not two that can drift" property
	// noteCreateRecord's own comment states for pins/surface/prompt above,
	// and shared with the idempotent-replay returns near the top of this
	// function via identityAssertionForCreate.
	return fleet.Session{
		SessionRef: ref, Cwd: spec.Cwd,
		Pins: pins, RuntimeSurface: surface, PromptDelivery: prompt,
		Conversation:      conversationCapturedRef(spec.ConversationId),
		IdentityAssertion: d.identityAssertionForCreate(name),
		Marker:            d.markerForCreate(name),
	}, nil
}

// claudeCodeCommand is the default CommandBuilder.
//
// Note what is absent: the prompt. It is delivered after the session is up
// (see Create) precisely so it stays out of this argv.
//
// # Why the remote-control flags are here by default
//
// They were missing, and their absence was invisible at creation: the session
// started, listed, read and drove perfectly, and was simply unreachable from
// any remote client. That is most of the value of creating one remotely in the
// first place — a session you must be sitting at the machine to use is a
// session you could have created by sitting at the machine.
//
// The binding is keyed on spec.Name, which Create has already resolved to the
// canonical string. The session name, the remote-control binding and the
// agent's own name are therefore the SAME string from birth, which is what
// makes a session findable by the one identifier every surface shows.
func claudeCodeCommand(spec fleet.SessionSpec, contextFile string) []string {
	argv := []string{"claude"}
	// A pin is data from a caller and lands in this argv beside real flags. A
	// value beginning with "-" is therefore not a value at all — the agent CLI
	// reads it as another flag, and a create grant becomes "run the agent with
	// arguments of my choosing". Nothing rejected these before; safeArgvValue
	// is applied to every caller-supplied element from here down.
	if spec.Resume != "" && safeArgvValue(string(spec.Resume)) {
		argv = append(argv, "--resume", spec.Resume)
	}
	// #224: the mirror of --resume above — a caller-chosen id to START a
	// conversation under, rather than one to continue. Create refuses the
	// combination of the two before argv is ever built (see the mutual-
	// exclusion check there), so at most one of these two ever appends.
	if spec.ConversationId != "" && safeArgvValue(spec.ConversationId) {
		argv = append(argv, "--session-id", spec.ConversationId)
	}
	// # Resuming pins the bridge the transcript remembers (#48)
	//
	// `--resume` and `--remote-control` interact in a way that only shows up
	// after something invalidates control channels fleet-wide. The transcript
	// records the channel id the session was bound to; resuming it makes the
	// runtime retry THAT id rather than mint a new one. If the id was orphaned
	// — a multiplexer death, a machine restart, anything that ends the old
	// worker — the retry can fail, and a session that resumed its conversation
	// perfectly comes back unreachable from outside.
	//
	// Measured: 63 sessions lost at once, 67 rebuilt with resume plus a
	// remote-control binding, 37 with no live channel. Retrying by hand
	// recovered 25 of them; 12 were refused permanently, the server having
	// archived the session — and for those the only way to mint a new channel
	// is to start WITHOUT `--resume`, which costs the conversation.
	//
	// Nothing here can prevent that; the trade is the caller's. What this
	// driver now does is stop hiding it: the runtime's own view of the channel
	// is reported as SessionState.ControlChannel (controlchannel.go), so a
	// supervisor can find the affected sessions by reading state instead of
	// grepping panes.
	//
	// Nil means "whatever a first-class session gets", which on this
	// substrate is enabled — an unaware caller must not silently receive the
	// second-class shape. Only an explicit false opts out.
	if spec.RemoteControl == nil || *spec.RemoteControl {
		if spec.Name != "" {
			argv = append(argv, "--remote-control", spec.Name, "-n", spec.Name)
		}
	}
	if spec.Agent != "" && safeArgvValue(string(spec.Agent)) {
		argv = append(argv, "--agent", string(spec.Agent))
	}
	if spec.Model != "" && safeArgvValue(spec.Model) {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" && safeArgvValue(spec.Effort) {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.PermissionMode == fleet.PermissionModeBypass {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	// muster #247/#254: launch-time settings, for any mode. The pairing of mode
	// and keys ValidateLaunchSettings enforces at Create is repeated here, so
	// this builder can never emit keys outside the boundary even if called with
	// an unvalidated spec — an invalid pairing emits nothing. Compact JSON is
	// one argv element beginning with "{", so it cannot be read as a flag.
	if settings, err := fleet.ValidateLaunchSettings(spec.PermissionMode, spec.Settings); err == nil && settings != "" {
		argv = append(argv, "--settings", settings)
	}
	for _, path := range spec.McpConfig {
		// Repeated rather than joined: the flag takes one path per occurrence,
		// and a joined list would be handed to the runtime as a single
		// filename containing a separator — a failure that surfaces as a
		// session missing its tools rather than as an error anyone can read.
		if safeArgvValue(string(path)) {
			argv = append(argv, "--mcp-config", string(path))
		}
	}
	if contextFile != "" {
		argv = append(argv, "--append-system-prompt-file", contextFile)
	}
	return argv
}

// validateMcpConfig refuses a create rather than starting a session that will
// come up without the tools it was asked for.
//
// # Why an unreadable path is a refusal and not a warning
//
// The runtime starts, fails to load the file, and presents a session that looks
// perfectly healthy: it lists, it reads, it accepts input. What it cannot do is
// the work it was created for, and nothing about it says so — the same shape as
// a session started without the environment holding its credentials, which is
// already refused here in the same words. A 201 for a session that is quietly
// not what was asked for is worse than an error at the call site.
//
// Contents are deliberately not read. A driver that parsed these would be
// deciding what a session may talk to, which is a supervisor's judgement (§1).
func validateMcpConfig(paths []fleet.AbsolutePath) error {
	for _, p := range paths {
		path := string(p)
		if path == "" {
			return errors.New("mcpConfig contains an empty path")
		}
		if !safeArgvValue(path) {
			return fmt.Errorf("mcpConfig %q would be read as a flag by the agent, not as a path", path)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("mcpConfig %q must be absolute; this service does not "+
				"resolve a path against a working directory it does not share", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("mcpConfig %q cannot be read here (%v); refusing rather than "+
				"starting a session that will come up without the tools it was created for", path, err)
		}
		_ = f.Close()
	}
	return nil
}

// safeArgvValue reports whether a caller-supplied value may be passed as an
// argv element.
//
// It answers one question: can this be mistaken for a flag? A leading "-" is
// the whole hazard — the agent CLI would read it as an option rather than as
// the value of the option before it, so a `model` of "--dangerously-skip-
// permissions" starts a session nobody asked for. The rest of the character set
// is left alone on purpose: these values never traverse a shell (the argv is
// exec'd directly, and the login wrap binds it as positional parameters), so
// quoting hazards do not arise and a stricter filter would only reject
// legitimate names this driver has no business vetting.
//
// Empty is not safe either: an empty element would silently pair the flag with
// whatever follows it.
func safeArgvValue(v string) bool {
	return v != "" && !strings.HasPrefix(v, "-")
}

// Respond answers a prompt the session is blocked on (§3).
//
// # It refuses when nothing is being asked
//
// A keypress delivered to a session that is not at a prompt lands in whatever
// that session was doing. Unlike a message — which at worst appears in a
// composer where a human can see and delete it — a stray keypress is consumed
// invisibly, and "Enter" against a composer holding a half-typed thought
// submits it.
//
// So this checks for a prompt first and refuses otherwise, which is §2.4's
// reasoning applied to control rather than to text.
//
// # Why this operation had to exist
//
// Three real prompts in one session made the case: a folder-trust question on
// every newly created session, a resume-from-summary question on a session
// being reattached, and a menu inside a running conversation. None could be
// answered through send(), because send() is built to guarantee it never
// produces a keystroke. A supervisor could start an agent and then not get
// past its first question — which is how a fleet loses a session to a dialog
// nobody can reach.
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	// Terminal path v2 / D4 — see Send's identical acquisition, and
	// lockComposerOpsCtx's own doc comment for why the caller's ctx governs
	// the wait rather than a bare, uninterruptible Lock(). Answering a
	// dialog is exactly the call this matters most for: it must not queue
	// for seconds behind an unrelated, unconfirmable send on the same
	// session.
	// #180 M4: a respond takes priority — a send holding the lock gives it
	// up at its next safe point, because the dialog this respond answers is
	// exactly what that send cannot get past.
	unlockComposer, lockOK := d.lockComposerPreempting(ctx, ref.ID)
	if !lockOK {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  composerBusyReason(""),
		}, nil
	}
	defer unlockComposer()

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	var target *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return d.noSuchSessionReceipt(ctx, rows, ref.ID), nil
	}
	if target.dead {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "session process has exited"}, nil
	}

	screenNow := captures[target.paneID].screen()
	before, shape := parsePromptShape(screenNow)
	unnumbered := shape.unnumbered
	if before == nil {
		// muster#215: the feedback-draft card over a composer row that holds
		// text is not a prompt (a digit would be appended to that text), and the
		// two refusals below would call it "not waiting on a prompt" or claim
		// there is no composer. Name it.
		if reason, isCard := feedbackNoticeRefusal(screenNow); isCard {
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
		}
		// muster #64: this refusal fires whenever the screen has no
		// structured prompt this driver recognises — which is right and
		// common (nothing is being asked) but was worded as though that
		// were the only possibility. A full-screen interface with no
		// composer either paints exactly the same way, and there "a
		// keypress would be consumed by whatever it is doing instead" is
		// false: what it is doing is waiting for that keypress.
		//
		// A composer being present settles it: the runtime is doing
		// something ordinary (idle, or a human mid-message), definitely
		// not blocked on an unrecognised full-screen prompt, since that
		// shape has no composer of its own to paint. A composerClipped
		// screen counts as present too (muster#134) — this driver's
		// capture not reaching the top of the composer is still positive
		// evidence a composer is painted there at all, which is the only
		// thing this check needs.
		if _, scan := composerText(screenNow); scan != composerAbsent {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "session is not waiting on a prompt; a keypress would be " +
					"consumed by whatever it is doing instead",
			}, nil
		}
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "no recognised prompt is on screen, and no composer either. If " +
				"the runtime is not asking anything, this refusal is the correct " +
				"one. If it is — a full-screen interface this driver did not " +
				"recognise as a structured prompt — respond() cannot answer it: " +
				"there is no option list or nonce here to answer against. keys() " +
				"can still reach the screen directly (deliversRawKeys: true)",
		}, nil
	}

	// A stale nonce means the caller is answering a question that is no
	// longer on screen. Submitting by index anyway would answer a DIFFERENT
	// question — and the two boot prompts on this substrate put the safe
	// option at different indices, so that is not a theoretical harm.
	if resp.Nonce != "" && resp.Nonce != before.Nonce {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the prompt changed since it was read; answering by index now " +
				"would answer a different question",
		}, nil
	}
	if resp.Choice > len(before.Options) {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "no such option on this prompt",
		}, nil
	}

	// muster#176: a multi-select question is answered with a set, and
	// only a multi-select question is. Everything here is decided from the
	// screen already read, before any key is sent.
	if err := resp.Validate(); err != nil {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: err.Error()}, nil
	}
	// muster#215: the runtime's feedback-draft card is answered by its own
	// keys, not by an option's number, and refuses most of what a menu accepts.
	// Everything about it is decided in one place.
	if shape.shortcuts != nil {
		return d.answerFeedbackCard(ctx, target.paneID, before, shape, resp)
	}
	boxes := 0
	if before.MultiSelect && !unnumbered {
		boxes = multiSelectBoxes(before)
	}
	switch {
	case len(resp.Choices) > 0 && boxes == 0:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "choices answers a multi-select question, and this prompt is not " +
				"recognised as one (prompt.multiSelect is not set); answer it with choice",
		}, nil
	case len(resp.Choices) > 0 || (resp.Text != nil && boxes > 0):
		// muster#206: on a multi-select question the free-text row is one
		// of the rows the boxes' own walk passes, so text rides the same path
		// — with or without boxes to tick alongside it.
		return d.answerMultiSelect(ctx, target, before, boxes, resp)
	case boxes > 0 && !resp.Cancel && resp.Choice <= boxes:
		// Measured: on a checkbox row a digit flips that one box and C-m
		// flips the highlighted one, and neither moves the dialog on. So a
		// single choice — or "accept the highlighted option" — answers
		// nothing, and before this refusal it was reported as submitted,
		// because the flipped tick changed the nonce.
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "this is a multi-select question (prompt.multiSelect): a single " +
				"choice, or accepting the highlighted row, would only flip one checkbox " +
				"and answer nothing. Send choices with every option that should end up " +
				"ticked",
		}, nil
	}

	// muster#206: an answer in the caller's own words, through the
	// free-text row. A multi-select question took it above; what reaches here
	// is a single-select one.
	if resp.Text != nil {
		return d.answerFreeText(ctx, target, before, resp)
	}

	// muster#204: a question drawn beside a preview pane takes its
	// answer in two keys, not one — a digit only moves the highlight there —
	// and the two must not be sent together. See answerPreview. Cancel is one
	// key on every layout and stays on the path below.
	if shape.preview && !resp.Cancel {
		return d.answerPreview(ctx, target.paneID, before, shape, resp)
	}

	// C-m rather than Enter — see confirmLanded for the measurement behind
	// this. A prompt that swallows the keypress leaves the session blocked,
	// which is the failure this operation exists to end.
	//
	// And `Space` before it, for the same reason as the submit in send(): the
	// first keystroke into an idle pane is swallowed when it is Enter, measured
	// 6 of 6. "Accept the highlighted option" is the one branch here that would
	// otherwise send a lone newline into exactly that slot — the `Choice > 0`
	// branch below already leads with a printable digit and never showed the
	// fault, which is itself corroboration. Escape is left alone: what was
	// measured is the Enter case, and guessing past the measurement is how the
	// wrong key ends up shipped.
	//
	// # A choice is the digit ALONE — the confirm is a fallback, not a pair
	//
	// muster#168. This branch used to send the digit and C-m together,
	// on the belief that the C-m either confirmed the digit or landed
	// harmlessly. Measured live on the runtime, a digit alone already commits
	// the answer on every numbered menu tried: a tabbed question of a
	// multi-question dialog (it also ADVANCES to the next tab), that dialog's
	// review confirm widget, and a single-question menu. So the C-m never
	// confirmed anything, and on a tabbed dialog it did harm: sent as one
	// burst on the second of three questions, the pair recorded that question
	// as its highlighted DEFAULT rather than the digit, left the third
	// unanswered, and landed on the review screen — while the nonce changed,
	// so this function reported success for an answer that was not given.
	//
	// So the digit goes alone, once, and nothing follows it. A same-prompt-
	// still-up result is reported as unknown, never retried here: re-sending
	// the digit spends the caller's answer twice, and the second lands in
	// whatever replaced the question the moment the first repaints (see
	// TestTrustConsentIsSpentOnce); a lone C-m confirms the HIGHLIGHTED
	// option, which on a menu where the digit changed nothing is not the one
	// chosen. Both would be a guess past the measurement. The caller holds
	// the unknown receipt and the screen, and can decide.
	//
	// # A menu with no numbers is walked to, then confirmed
	//
	// muster#171, measured on the runtime's unnumbered folder-trust
	// menu: a digit changed nothing (the screen stayed byte-identical), Down
	// moved the highlight, Space changed nothing, and C-m confirmed the
	// highlighted row. So there the choice is delivered as arrow presses,
	// then a READ proving the highlight sits on the chosen row, and only
	// then C-m. If the highlight did not arrive, nothing is confirmed: C-m
	// would accept whichever row it did stop on, which is not the one chosen.
	// A choice that is already highlighted needs no walk and takes the
	// accept-highlighted keys, since Space is measured inert there.
	keys := []string{"Space", "C-m"}
	switch {
	case resp.Cancel:
		keys = []string{"Escape"}
	case resp.Choice > 0 && unnumbered:
		if resp.Choice != before.Selected {
			arrived, reason, err := d.walkHighlight(ctx, target.paneID, before, resp.Choice)
			if err != nil {
				return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
			}
			if !arrived {
				return fleet.DeliveryReceipt{Outcome: fleet.OutcomeUnknown, Reason: reason}, nil
			}
			keys = []string{"C-m"}
		}
	case resp.Choice > 0:
		keys = []string{strconv.Itoa(resp.Choice)}
	}
	args := []string{"send-keys", "-t", target.paneID}
	args = append(args, keys...)
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}

	// Confirm the prompt actually went away. A keypress a prompt swallows
	// leaves the session exactly as stuck as before, and reporting success
	// there is how a supervisor concludes it has cleared something it has
	// not.
	cleared := d.promptCleared(ctx, target.paneID, before.Nonce)

	answered := "accepted the highlighted option"
	switch {
	case resp.Cancel:
		answered = "cancelled the prompt"
	case resp.Choice > 0:
		answered = "chose option " + strconv.Itoa(resp.Choice)
	}
	if resp.Choice > 0 && resp.Choice <= len(before.Options) {
		answered += " (" + before.Options[resp.Choice-1] + ")"
	} else if before.Selected > 0 && before.Selected <= len(before.Options) {
		answered += " (" + before.Options[before.Selected-1] + ")"
	}
	if resp.Nonce == "" {
		answered += "; answered without a nonce, so nothing verified the prompt " +
			"had not changed since it was read"
	}

	if cleared {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: answered}, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason:  answered + "; the prompt is still on screen, so the keypress may not have registered",
	}, nil
}

// settleNewSession carries a freshly created session from "the process is
// spawned" to "it is doing the work it was created for": it waits for the
// runtime to be ready and then sends §2.1's initial prompt, and — only when the
// caller asked for it — answers the folder-trust question the runtime puts in
// front of that.
//
// # Why this cannot happen inside Create
//
// §2.1 says a spec may carry an initial prompt, and §4.4 says every call is
// bounded by the driver's declared deadline. On this substrate those two do
// not fit: the runtime takes far longer to paint its interface than any sane
// per-call deadline, so a Create that waited would violate its own
// declaration, and a Create that did not wait delivered into a terminal that
// was not listening.
//
// The second is what happened, and it is worse than it sounds. The paste
// landed but the submit keystroke was swallowed during startup, leaving the
// prompt sitting UNSENT in the composer — indistinguishable from a human's
// half-typed message, so every later send to that session was refused in order
// to protect text the session had put there itself. Create manufactured
// exactly the stuck session this driver exists to avoid.
//
// So delivery happens after Create returns, and only once the interface is
// ready to receive. Failure is not silent: the prompt is simply absent, and
// the session's state says what it is doing instead.
//
// # A blocking question is waited THROUGH, not given up on
//
// This loop used to return the moment it saw any prompt, on the reasoning that
// answering one is a decision it does not hold. That reasoning is still right,
// and returning was still wrong: the two are separate acts. A human who answers
// the trust question ten seconds later gets a session that is ready, willing,
// and holding no work — because the instruction it was created with was
// discarded while the modal was up, and nothing anywhere records that it
// existed. Measured on a live fleet: a session parked on that question for two
// days, and the work it was spawned for nowhere.
//
// So a prompt this routine may not answer is now a reason to keep waiting, not
// a reason to stop.
//
// # muster #125: bounded by the session's own lifetime, not a guessed duration
//
// This used to give up after a fixed 90s window, on the theory that a session
// still not ready by then had probably lost its chance. #125 measured the cost
// of that theory being wrong: a session parked on a dialog is answerable in
// ninety seconds or in ten minutes, entirely depending on when a human notices
// it — a duration this service cannot predict and has no business guessing at.
// Racing an arbitrary timer against an unknown human response time discards the
// caller's entire instruction on exactly the sessions most likely to still be
// alive and worth delivering to.
//
// So there is no timer here at all. This loop tries as hard as possible: it
// keeps polling for as long as the session itself exists, and stops only on
// one of three real events — delivered, refused, or the session itself is
// gone (promptReadiness's own `present` signal, confirmed over
// sessionGoneConfirmations consecutive polls so a single listing race is not
// mistaken for a closed session). "The session's own lifetime" is the bound;
// nothing here waits longer than the thing it is waiting on.
//
// # muster #125: an answer to WHY, available DURING the wait
//
// A design that merely retries harder while staying silent trades one
// invisible failure for another — a session that quietly did nothing becomes a
// service that quietly waits, and a human staring at either gets the same
// nothing. So every poll that finds the session still not ready records ITS
// OWN reason on the create record (notePromptPending, below the switch) —
// "still starting", "parked on a folder-trust dialog awaiting a keypress",
// "the composer already holds other text" — not only a static "pending" flag.
// A caller reading the session mid-wait sees a diagnosis, not a mystery; #86's
// terminal-outcome guarantee (never `null`, always a real value once delivery
// is abandoned) is unchanged and sits alongside it, not instead of it.
//
// # muster #126: that reason is now a class too, not only prose
//
// The prose above is exactly what a human reads; it is not something a caller
// can branch on. promptReadiness's readinessCheck.waitingOn carries the SAME
// classification the prose is built from — a dialog is fleet.WaitingPrompt, a
// composer holding other text is fleet.WaitingUnsentInput, no composer
// painted at all is fleet.WaitingStarting — and rides along on the identical
// call to notePromptPending, so PromptDelivery.WaitingOn and Evidence can
// never disagree about the same wait.
func (d *Driver) settleNewSession(req fleet.Request, ref fleet.SessionRef, spec fleet.SessionSpec) {
	ctx := context.Background()

	// One consent, spent once — per kind, because a session can meet more than
	// one boot question on the way up. A question re-read on the next poll,
	// because the keypress has not repainted yet, must not be answered twice:
	// the second digit lands in whatever screen replaced it.
	answered := map[fleet.PromptKind]bool{}
	consecutiveGone := 0
	lastEvidence := ""
	for {
		check := d.promptReadiness(ctx, ref.ID)

		if check.checked && !check.present {
			consecutiveGone++
		} else {
			consecutiveGone = 0
		}
		if consecutiveGone >= sessionGoneConfirmations {
			// #125: the session this delivery targeted is gone, so nothing
			// will ever become ready to receive it — that is a real, terminal
			// answer, not a reason to keep polling a session that no longer
			// exists. #86's own rule still applies: resolved as unknown, never
			// left unresolved, because nothing DECLINED this delivery, the
			// target simply stopped existing first.
			if spec.Prompt != "" {
				d.counters.incr(counterInitialPromptSessionGone)
				d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown,
					"the session no longer exists; it ended before this driver "+
						"could deliver the prompt it was created with")
			}
			return
		}

		blocking := check.blocking
		// An unclassified screen may still be one this driver can identify —
		// not from what it says, but from what this driver did to produce it.
		// See acceptanceScreen.
		if blocking != nil && blocking.Kind == "" && acceptanceScreen(spec, blocking) {
			blocking.Kind = fleet.PromptBypassAcceptance
		}
		switch {
		case blocking != nil && spec.ConsentsTo(blocking.Kind) && !answered[blocking.Kind]:
			// The caller described this session in the create request — its
			// working directory, its permission mode — and consented, in the
			// same request, to the runtime's boot question about what it
			// described. The decision is the caller's; this is only its
			// execution, which is the line prompt.go draws when it says a
			// service that decided what to answer would have become a
			// supervisor.
			if choice, ok := affirmativeOption(blocking); ok {
				answered[blocking.Kind] = true
				_, _ = d.Respond(ctx, req, ref, fleet.Response{
					Choice: choice, Nonce: blocking.Nonce,
				})
			}
		case check.ready:
			if spec.Prompt != "" {
				d.deliverInitialPrompt(ctx, req, ref, spec.Prompt)
			}
			return
		default:
			// Still waiting. #125's live half: record WHY, not only THAT —
			// only on change, so a long wait costs one write per state
			// transition, not one every promptPollInterval.
			if spec.Prompt != "" && check.reason != "" && check.reason != lastEvidence {
				d.notePromptPending(ref.ID, check.waitingOn, check.reason)
				lastEvidence = check.reason
			}
		}
		time.Sleep(promptPollInterval)
	}
}

// deliverInitialPrompt sends §2.1's initial prompt and, unlike the single
// discarded call this replaced, does something with what Send tells it.
//
// # What this receipt could never do before
//
// Create already returned 201 by the time this runs, so nobody is waiting on
// this specific call the way an ordinary caller waits on Send's HTTP
// response. That is what made the old `_, _ = d.Send(...)` different from
// every other place this driver ignores nothing: it was not indifference to
// a receipt, it was the one receipt with no possible reader. #44 measured
// what that cost — 3 of 5 multi-line initial prompts stranded in a single
// run, all recovered by a human who happened to read the session's own
// state and call the resume path, none of the six ever told without being
// asked.
//
// # One retry, inside the window, through the path already proven correct
//
// #44's own measurement is why this is a retry and not a redesign: every
// strand in that run cleared on the very next attempt, seconds later, with
// nothing different but time — consistent with a busy machine's repaint
// racing the submit keystroke, worse as load grows, not a defect in the
// delivery logic. And a retry already has somewhere honest to go: Send's
// first attempt calls noteStranded on any unconfirmed outcome, exactly as it
// does for a caller-initiated send, so a second Send with ResumeIfStranded
// walks the same recovery path a human used by hand — the identical
// mechanism, not a parallel one built for this call site.
//
// A retry that silently succeeds would hide the one number #44 says matters:
// how often this needs to happen at all. counterInitialPromptRetried is
// incremented on every retry regardless of its outcome, so a machine
// clearing every strand on retry still shows up in the count — that rate is
// the load signal, not something to launder away by only counting failures.
//
// # What still reaches #11 unchanged, and what does not
//
// noteStranded's record lives in d.stranded, which #11 already tracks as
// in-memory and restart-fragile. This function adds no new writer to that
// map — both the first attempt and the retry go through the same Send this
// driver's other callers already use, so this call site is not a third
// writer of anything. What it DOES do is call Send up to twice where the
// discarded version called it once, so a machine under exactly the load
// pattern #44 measured now writes that entry twice as often on the losing
// side of the race before either clearing it (resumeIfStranded's own
// forgetStranded) or giving up. That is a real cost of retrying and is
// recorded here rather than left for #11 to discover on its own.
//
// # Why the second failure is a log line and a counter, not a new event kind
//
// events.go's EventKind is `api-http.md §4`'s closed, normative set, and this
// driver has no channel into the hub outside the subscription engine's own
// poll-diff loop (internal/service/events.go). Inventing a delivery-specific
// event here would mean a spec change and a new cross-package wire this
// function has no business owning. It is also not the only way a
// subscriber learns: the composer is, physically, still holding the text,
// so the very next classify of this pane reports `waiting_input` with
// `WaitingOn: unsent-input` — the same read #44 measured working 6 times out
// of 6 for detection — and the subscription engine already emits
// `session.state` the moment that status differs from what a live
// subscriber last saw, with no code added here. What was missing was never
// the detection path; it was that Create's caller has no reason to be
// looking. counterInitialPromptStranded and the log line exist for the
// caller who is not subscribed and never will be — an operator asking
// afterwards "did this happen, how often" — which is exactly the shape #9
// describes wanting and not having.
func (d *Driver) deliverInitialPrompt(ctx context.Context, req fleet.Request, ref fleet.SessionRef, prompt string) {
	// #184: the create-time prompt is the creator's launch instruction, not a
	// peer's message, so it is pinned to the terminal — on the first attempt
	// AND the retry. That is also what makes the retry below safe: it can only
	// resume something the terminal path itself stranded, never re-send text the
	// inbox already put in front of the receiver.
	receipt, err := d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true, Route: fleet.RouteTerminal})
	if err != nil {
		// A transport-level error Send itself already wraps and named. #86:
		// resolved as unknown, not refused — nothing declined this delivery,
		// the attempt itself failed.
		d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown, err.Error())
		return
	}
	if receipt.Outcome != fleet.OutcomeUnknown {
		// Delivered and confirmed, or refused outright — not this call's
		// problem to retry into. #86: resolved with the receipt's own
		// outcome and reason, the same pair send() itself would have
		// answered with had anyone been waiting on this specific call.
		evidence := receipt.Reason
		if evidence == "" {
			evidence = "the driver confirmed the agent received the create-time prompt"
		}
		d.notePromptDelivered(ref.ID, receipt.Outcome, evidence)
		return
	}

	d.counters.incr(counterInitialPromptRetried)
	retry, err := d.Send(ctx, req, ref, prompt, driver.SendOptions{Submit: true, ResumeIfStranded: true, Route: fleet.RouteTerminal})
	// #101: the resume path used to report `submitted` unconditionally, which
	// is why this compared against it. It now confirms the submit the same
	// way the first attempt does and reports `queued` on the same evidence
	// (§4.3 — this substrate cannot observe agent receipt on either path).
	// Comparing against `queued` here keeps this call site in step with that
	// contract instead of silently degrading to "always retry failed" the
	// moment resume stopped over-claiming.
	if err == nil && retry.Outcome == fleet.OutcomeQueued {
		// #86: delivered on the retry — a different fact from an ordinary
		// first-attempt submission, worth saying so in the evidence rather
		// than reporting it identically.
		d.notePromptDelivered(ref.ID, fleet.OutcomeQueued,
			"delivered on the second attempt; the first could not be confirmed in time")
		return
	}

	// Still sitting there after the one retry the measured pattern earns it.
	// The text and the record of it are exactly where an ordinary stranded
	// send leaves them (composer, d.stranded) — nothing here is lost, only
	// unannounced, which this closes.
	d.counters.incr(counterInitialPromptStranded)
	log.Printf("tmux: initial prompt still unsent after one retry session=%s machine=%s",
		ref.ID, d.machine)
	// #86: resolved as unknown — the text reached the composer and could
	// not be confirmed submitted after one retry; it is sitting there
	// unsent, exactly as the log line above and d.stranded already record,
	// now also readable from the create response without a log to grep.
	// #180 M1: say what the retry actually found, not a guess. A retry can
	// end unknown for more than one reason — the text sitting unsent is one,
	// a transcript that could not confirm is another — and the old fixed
	// sentence asserted the first whatever happened.
	why := "the retry failed"
	switch {
	case err != nil:
		why = "the retry failed: " + err.Error()
	case retry.Reason != "":
		why = "the retry answered " + string(retry.Outcome) + ": " + retry.Reason
	}
	d.notePromptDelivered(ref.ID, fleet.OutcomeUnknown,
		"the create-time prompt could not be confirmed delivered after one retry. The first "+
			"attempt answered unknown: "+receipt.Reason+" — and "+why)
}

// consentableKinds maps each boot question a caller may consent to onto the
// words that identify its affirmative option.
//
// # Why the resume chooser is absent, and must stay absent
//
// It is the obvious fourth entry and it has no safe answer. The other two ask a
// yes/no about something the caller DESCRIBED in its own create request — this
// directory, this permission mode — so "the option that agrees" is a fact about
// the screen. The resume chooser asks WHICH conversation to continue, and its
// options are summaries of somebody's prior sessions. Nothing in the option text
// identifies the one the caller named; a consent here would be a coin flip
// dressed as an agreement, and losing it resumes a stranger's work.
//
// A caller that wants it answered reads `state.prompt` and answers by index
// through `respond`, which is exactly the split prompt.go describes: this
// service says what is being asked, a supervisor decides what to answer.
//
// # PromptSettingsTrust is absent too, for a third reason
//
// Its affirmative agrees to an ADMINISTRATOR's managed-policy payload, not to
// anything the caller described in its own request — see prompt.go's doc
// comment on the kind. A consent here would let a session-creating caller
// accept a policy change on behalf of an operator who never saw it, which
// this layer is not in a position to speak for. Absence from this map is
// enough: the loop in Create refuses any Consents entry with no map key, the
// same mechanism that refuses PromptResumeChooser above.
var consentableKinds = map[fleet.PromptKind][]string{
	// "Yes, I trust this folder"
	fleet.PromptFolderTrust: {"trust", "folder"},
	// "Yes, allow external imports". The decline reads "No, disable external
	// imports" and shares two of the three words, so it is "allow" that
	// isolates the affirmative — and a rewording that puts "allow" in both rows
	// is the ambiguity affirmativeOption already refuses to guess through.
	// Answered by index: this question's highlight defaults to the decline
	// (muster #211).
	fleet.PromptExternalImports: {"allow", "external", "imports"},
	// PromptBypassAcceptance is consentable but has NO entry here, because its
	// affirmative option is the generic "Yes, I accept" and this table is
	// applied to whatever screen happens to be on the pane. A needle that loose
	// would accept some future dialog nobody has seen. It is resolved by
	// provenance instead — see acceptanceScreen.
	fleet.PromptBypassAcceptance: nil,
}

// affirmativeOption picks the option that AGREES, by index.
//
// It reads the option text and nothing else, for the reason classifyPromptKind
// states at length: options are strings the runtime emits, while the question
// is written by the agent and is therefore injectable. And it does not fall
// back to the highlighted option — prompt.go's own example is two boot prompts
// with the same shape whose safe answer sits at different indices:
//
//	❯ 1. Yes, I trust this folder        ❯ 1. No, exit
//	  2. No, continue without these        2. Yes, I accept
//
// Exactly one option may match. Zero means this is not the screen we were told
// about; two means the wording has changed under us (a "No, I don't trust this
// folder" matches the same needles), and in both cases the honest move is to
// answer nothing and leave a question on screen for a human — the same direction
// §5.6 sends every other unreadable case.
// acceptanceScreen reports whether an unclassified boot screen is the
// permission-mode acceptance one, using evidence the screen cannot supply.
//
// # Identify it by what we did, not by what it says
//
// The classifier cannot name this screen: its identifying words are in the
// question, its options are the generic "Yes, I accept" / "No, exit", and
// reading questions is the thing option-matching exists to avoid. Loosening the
// classifier to match "accept" would put this kind on screens nobody has seen,
// in a package whose kinds are used by clients to decide what to auto-answer.
//
// But this driver is not a bystander here. It PASSED the flag that raises this
// screen, moments ago, to this session. That is provenance, it is unavailable
// to the classifier, and it is far stronger evidence than any wording: an agent
// can print any sentence it likes into its own prompt, and cannot cause the
// driver to have started it in a mode it was never asked for.
//
// Three conditions, all required:
//
//   - the caller asked for the mode whose acceptance screen this is;
//   - the caller consented to this kind in the same request;
//   - the screen is a two-option accept/decline pair, matched on OPTION text
//     only, which is the material the invariant does allow.
//
// The last one is what keeps a coincidence from being read as agreement. It
// deliberately does not identify the screen on its own — only in the presence
// of the first two, which no agent-authored prompt can manufacture.
func acceptanceScreen(spec fleet.SessionSpec, p *fleet.SessionPrompt) bool {
	if spec.PermissionMode != fleet.PermissionModeBypass {
		return false
	}
	if !spec.ConsentsTo(fleet.PromptBypassAcceptance) {
		return false
	}
	if p == nil || len(p.Options) != 2 {
		return false
	}
	accepts, declines := 0, 0
	for _, o := range p.Options {
		lower := strings.ToLower(o)
		if strings.Contains(lower, "accept") {
			accepts++
		}
		if strings.Contains(lower, "exit") || strings.Contains(lower, "no,") {
			declines++
		}
	}
	return accepts == 1 && declines == 1
}

func affirmativeOption(p *fleet.SessionPrompt) (int, bool) {
	if p == nil {
		return 0, false
	}
	needles, ok := consentableKinds[p.Kind]
	if !ok {
		return 0, false
	}
	// A consentable kind with no needles is one identified by provenance rather
	// than by wording (see acceptanceScreen). Its affirmative is the option that
	// accepts — the same options-only material, applied to a screen we already
	// have independent grounds to believe we are looking at.
	if needles == nil {
		needles = []string{"accept"}
	}
	found := 0
	for i, o := range p.Options {
		lower := strings.ToLower(o)
		// No "does it start with no" refinement. That is guessing at wording
		// in order to keep answering a screen we have just been told we can no
		// longer read — the ambiguity below is the answer, not an obstacle.
		all := true
		for _, n := range needles {
			if !strings.Contains(lower, n) {
				all = false
				break
			}
		}
		if all {
			if found != 0 {
				return 0, false
			}
			found = i + 1
		}
	}
	return found, found != 0
}

// readinessCheck is promptReadiness's full answer. #125 asks for two things a
// bare (ready, blocking) pair cannot carry: whether the session is even still
// there to wait on, and a live, human-readable reason for whatever state was
// found — settleNewSession's own diagnosis of "still starting" vs "parked on
// a dialog" vs "the composer holds something else" comes from here, not from
// re-deriving it against the raw screen a second time.
type readinessCheck struct {
	// ready means the composer exists and is empty: the interface has
	// painted, and nothing is already sitting in it.
	ready bool
	// blocking is the question the session is parked on, when one is on
	// screen — nil otherwise. Returned as a value rather than folded into
	// `reason` because settleNewSession's own consent logic needs the
	// structured Kind/Options/Nonce, not prose.
	blocking *fleet.SessionPrompt
	// present reports whether this session was found at all in the
	// enumeration this check ran, INCLUDING a pane whose process has already
	// exited (tmux's own dead flag) — both mean nothing will ever become
	// ready here. Only meaningful when checked is true.
	present bool
	// checked reports whether the enumeration itself succeeded. False means
	// this check learned nothing — a transient listing failure, not evidence
	// the session is gone (see the "pane can vanish between listing and
	// capture" caution elsewhere in this file) — and present must not be
	// read in that case.
	checked bool
	// reason is prose for a human, populated whenever checked is true. Never
	// parsed (§2.3) — the same discipline every other Evidence field in this
	// package holds itself to.
	reason string
	// waitingOn (muster #126) is the machine-readable class for reason,
	// computed by the SAME branch that produces the prose so the two can
	// never disagree — settleNewSession passes it straight through to
	// notePromptPending. Empty on `ready` (nothing to wait on any more) and
	// on a check this driver could not classify further than "checked but
	// unhelpful" (the session was not visible, or its process had already
	// exited) — unclassified there, never a guess.
	waitingOn fleet.WaitingReason
}

func (d *Driver) promptReadiness(ctx context.Context, id string) readinessCheck {
	callCtx, cancel := d.bounded(ctx)
	defer cancel()
	rows, captures, err := d.enumerate(callCtx)
	if err != nil {
		return readinessCheck{}
	}
	for _, r := range rows {
		if r.session != id {
			continue
		}
		if r.dead {
			return readinessCheck{checked: true,
				reason: "the session's process has already exited"}
		}
		sc := captures[r.paneID].screen()
		if p := parsePrompt(sc); p != nil {
			p.Kind = classifyPromptKind(p)
			reason := "parked on a prompt this driver does not recognise; " +
				"a human may need to look at the session directly"
			if p.Kind != "" {
				reason = fmt.Sprintf("parked on a %q dialog awaiting a keypress", string(p.Kind))
			}
			if p.Question != "" {
				reason += fmt.Sprintf(" (%q)", p.Question)
			}
			return readinessCheck{checked: true, present: true, blocking: p, reason: reason,
				waitingOn: fleet.WaitingPrompt}
		}
		text, scan := composerText(sc)
		if scan == composerClipped {
			// muster#134: a composer is painted, but taller than this
			// driver's capture window — this driver cannot confirm it is
			// empty, so it must not report ready. Fail closed the same
			// direction WaitingUnsentInput already means: something may be
			// sitting there this call cannot see.
			return readinessCheck{checked: true, present: true,
				reason: "the composer " + composerClippedCause(sc) + "; " +
					"cannot confirm it is empty before placing this prompt",
				waitingOn: fleet.WaitingUnsentInput}
		}
		if scan != composerFound {
			return readinessCheck{checked: true, present: true,
				reason:    "still starting: the interface has not painted a composer yet",
				waitingOn: fleet.WaitingStarting}
		}
		if text != "" {
			return readinessCheck{checked: true, present: true,
				reason: "the composer already holds other text; waiting for it to " +
					"clear before this prompt can be placed",
				waitingOn: fleet.WaitingUnsentInput}
		}
		return readinessCheck{checked: true, present: true, ready: true,
			reason: "the composer is empty and ready"}
	}
	return readinessCheck{checked: true,
		reason: "the session is not visible to this driver right now"}
}

// pasteKey identifies one collapsed-paste summary well enough to tell it from
// the one before it.
//
// Index is the runtime's own counter ("#10"), which is monotonic within a
// session and is therefore the strong identifier. It is optional: a runtime
// that prints only a line count still gets attribution, because a SECOND block
// of the same size is a second entry under the same key and the counting below
// notices the difference. What is not optional is the line count — it is the
// only thing the summary exists to say.
type pasteKey struct{ index, lines int }

// markerCounts is the composer's paste state: how many collapsed blocks it
// shows, of which shapes.
//
// # Why a count of shapes rather than "is a marker present"
//
// A marker says "this composer holds a pasted block". It does NOT say the block
// is the one just delivered, and both confirmations used to read it as if it
// did. Any earlier stranded paste satisfies the same test, so a send into a
// composer with residue confirmed against somebody else's leftovers, and the
// submit check — which watched for the composer to EMPTY — could then never
// pass, because the residue never leaves.
//
// The compounding is what made it urgent. The receipt's own advice on failure
// is "retry with resumeIfStranded", the retry pastes again, the composer is
// less empty than before, and the next receipt is wrong for the same reason:
// the state degrades because the caller did what the receipt told it to.
// Measured on a live session, one ~30-line instruction produced two consecutive
// false negatives while the agent had received exactly one clean copy and was
// acting on it, with the composer holding two placeholders.
//
// So the unit of evidence is the CHANGE in this map across a delivery, never
// the presence of a marker.
func markerCounts(painted string) map[pasteKey]int {
	out := map[pasteKey]int{}
	for _, line := range strings.Split(painted, "\n") {
		if !strings.Contains(line, composerRuneMarker) {
			continue
		}
		rest := line[strings.Index(line, composerRuneMarker):]
		for {
			open := strings.Index(rest, "[")
			if open < 0 {
				break
			}
			shut := strings.Index(rest[open:], "]")
			if shut < 0 {
				break
			}
			inside := rest[open+1 : open+shut]
			rest = rest[open+shut:]
			lower := strings.ToLower(inside)
			// Review fix (single-line pastes over ~800 bytes never confirm
			// landed): a COLLAPSED SINGLE-LINE paste renders as bare
			// "Pasted text #N" — no "+K lines" suffix at all, because there
			// is only the one line to summarise. The original check required
			// both "line" AND a digit, so this exact shape — round-1's own
			// 900/1500/1900-byte single-line probes — was silently skipped
			// here, confirmLandedV2 never saw a marker gain, and the
			// delivery could never be confirmed landed at any length past
			// the collapse threshold. "Pasted text #<digits>" alone is
			// accepted now, with lines defaulting to 0 (numberAfter's own
			// convention for a missing count, matching how a multi-line
			// marker with no printed count was already treated) — the two
			// shapes are told apart by whether "#" is followed by digits at
			// all, not by requiring the word "line" to be present.
			hasIndex := strings.Contains(inside, "#") && strings.ContainsAny(lower, "0123456789")
			hasLineCount := strings.Contains(lower, "line")
			if !hasIndex && !hasLineCount {
				continue
			}
			out[pasteKey{index: numberAfter(inside, '#'), lines: numberAfter(inside, '+')}]++
		}
	}
	return out
}

// numberAfter reads the run of digits following a marker rune, or 0 when the
// rune is absent. A runtime that stops printing "#" loses the strong
// identifier and keeps the weak one; it does not lose attribution entirely.
func numberAfter(s string, marker byte) int {
	i := strings.IndexByte(s, marker)
	if i < 0 {
		return 0
	}
	n, digits := 0, 0
	for j := i + 1; j < len(s) && s[j] >= '0' && s[j] <= '9'; j++ {
		n = n*10 + int(s[j]-'0')
		digits++
		if digits > 6 {
			return 0 // not a count; this pane is attacker-influenced text
		}
	}
	if digits == 0 {
		return 0
	}
	return n
}

// gained reports the key whose count went UP between two readings — the block
// this delivery is responsible for.
//
// Ambiguity fails to "no attribution" rather than to a guess: if two keys grew,
// something other than this delivery also wrote to the composer, and claiming
// either would be the same unfounded confidence this whole change removes.
func gained(before, after map[pasteKey]int) (pasteKey, bool) {
	var found pasteKey
	hits := 0
	for k, n := range after {
		if n > before[k] {
			found = k
			hits++
		}
	}
	return found, hits == 1
}

// composerHoldsCollapsedPaste reports whether the composer line shows the
// runtime's summary of a pasted block rather than the pasted text itself.
//
// Kept for the readers that legitimately want "is there any pasted block here"
// — the §2.4 refusal cares about that, because residue is still text a send
// would concatenate with. Confirmation of a DELIVERY must not use it; that is
// what markerCounts and gained are for.
func composerHoldsCollapsedPaste(painted string) bool {
	return len(markerCounts(painted)) > 0
}

// noteQuotaBlock remembers that this machine's account is refusing work, and
// forgets it the moment something proves otherwise.
//
// Called with what the current read saw: whether any session's SCREEN showed
// a limit notice (sawLimit/hint — upgrade-only per #54: this is what may
// promote the account INTO a block, never what clears one) and whether any
// session was observed working (sawWorking).
//
// record and verdict are #56's structured source: the runtime's own record
// for whichever session the current read found already reporting
// quota_blocked, when one could be resolved. Three things follow from it —
//
//   - recordAPIError with category "rate_limit" corrects Since to the
//     refusal's own timestamp, and ResetHint to the record's own words,
//     rather than trusting the screen's window-scraped copy of the same
//     sentence.
//   - recordCleanTurn is durable, positive proof the account is not
//     refusing work — the same authority sawWorking already has, from a
//     source that survives the notice scrolling off the one pane that
//     showed it. It clears the block alongside sawWorking, never in place
//     of it: a driver with no record store configured must keep exactly
//     today's behaviour.
//   - recordUnavailable changes nothing here; the caller falls back to
//     first-sighting semantics and says so in SessionState.Evidence
//     (quotaBlockedState), never presenting one as the other.
func (d *Driver) noteQuotaBlock(sawLimit bool, hint string, sawWorking bool, record apiErrorFact, verdict recordVerdict, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case sawWorking, verdict == recordCleanTurn:
		if d.quota != nil {
			d.quota = nil
			d.quotaSinceObserved = false
			d.saveQuotaLocked()
		}
	case sawLimit && d.quota == nil:
		since, sinceObserved, resetHint := now, false, hint
		if verdict == recordAPIError && record.category == "rate_limit" {
			since, sinceObserved = record.at, true
			if h := record.resetHintText(); h != "" {
				resetHint = h
			}
		}
		d.quota = &fleet.QuotaBlock{Since: since, ResetHint: resetHint}
		d.quotaSinceObserved = sinceObserved
		d.saveQuotaLocked()
	case sawLimit && verdict == recordAPIError && record.category == "rate_limit" && !d.quotaSinceObserved:
		// A block already exists from an earlier cycle's first sighting,
		// and the record has only now resolved (a conversation lookup can
		// legitimately lag a cycle, or the block was carried in from a
		// restart before a record was ever consulted). Upgrade Since in
		// place rather than waiting for the block to clear and re-enter.
		d.quota.Since = record.at
		d.quotaSinceObserved = true
		if h := record.resetHintText(); h != "" && d.quota.ResetHint == "" {
			d.quota.ResetHint = h
		}
		d.saveQuotaLocked()
	case sawLimit && hint != "" && d.quota.ResetHint == "":
		// A later notice may carry a reset time the first one did not.
		d.quota.ResetHint = hint
		d.saveQuotaLocked()
	}
}

// quotaBlock reports the remembered account block, if any, and whether its
// Since is the runtime's own record of the refusal rather than this
// driver's first sighting of the notice (#56) — see quotaBlockedState for
// where that distinction becomes something a caller can read.
func (d *Driver) quotaBlock() (*fleet.QuotaBlock, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quota == nil {
		return nil, false
	}
	q := *d.quota
	return &q, d.quotaSinceObserved
}

// quotaOnly discards quotaBlock's sinceObserved half for call sites that
// only need the wire fact — fleet.SourceStatus.Quota carries no evidence
// string of its own to put the distinction in (unlike a session's own
// SessionState, which quotaBlockedState annotates).
func quotaOnly(q *fleet.QuotaBlock, _ bool) *fleet.QuotaBlock { return q }

// credentialGeneration reads the local credential store's own modification
// time — the "generation" identifier #12 needs — or nil when unconfigured
// or the stat fails.
//
// One stat call, every read. No lock and no field on Driver remembers a
// value across calls, unlike quotaBlock: a file's mtime does not evaporate
// the way a scrolled-away screen notice does, so this driver's own memory
// would only be a second, potentially stale copy of what one syscall
// already answers fresh. The finding this exists to surface was itself made
// this way, from outside the process entirely — this is that same cheap
// signal, already within reach, not a new detector.
//
// It answers "which generation is in force right now", never "is a session
// still bound to it" — see SessionState.CredentialGeneration for the
// distinction this return value must not blur.
func (d *Driver) credentialGeneration() *fleet.Timestamp {
	if d.credentialPath == "" {
		return nil
	}
	info, err := os.Stat(d.credentialPath)
	if err != nil {
		return nil
	}
	t := info.ModTime()
	return &t
}

// quotaBlockedState rewrites st to quota_blocked when q is non-nil and st's
// own status is silence about the account rather than evidence about it —
// generalising List's per-session rewrite so State (a single-session read)
// applies the identical rule (#10).
//
// Three statuses qualify, and the third is the one List never needed to
// consider on its own. idle and unknown are exactly List's original two —
// see its own comment for why. starting is idle's condition at the earliest
// possible moment: a session this driver just spawned, for an account
// already known to be refusing work, paints nothing yet — and "starting" is
// what a caller reads for it. That caller is very often the one who just
// called Create, reading the state embedded in its 201 response (built from
// this exact function, by way of Driver.State) — the response #10 exists to
// keep honest. Swallowing the account fact behind "starting" until the next
// poll notices "idle" would report an unqualified success, which is exactly
// what #10 says a create must not do.
//
// Nothing else is rewritten, unchanged from List: working, waiting_input and
// unsent text each carry something OBSERVED just now, and a remembered fact
// must not overwrite an observation.
//
// sinceObserved says which source q.Since came from (#56's quotaBlock
// accessor) and is spelled out in Evidence rather than added as a new field
// on the wire QuotaBlock — the same "say so in evidence, do not add a
// silent field" rule this issue was written to enforce.
func quotaBlockedState(st fleet.SessionState, q *fleet.QuotaBlock, sinceObserved bool) fleet.SessionState {
	if q == nil {
		return st
	}
	var seen string
	switch st.Status {
	case fleet.StatusIdle:
		seen = "the session itself looks idle"
	case fleet.StatusUnknown:
		seen = "the session's own screen was inconclusive"
	case fleet.StatusStarting:
		seen = "the session had not yet painted anything of its own"
	default:
		return st
	}
	st.Status = fleet.StatusQuotaBlocked
	st.Quota = q
	st.Evidence = "this machine's account is refusing work; " + seen
	if sinceObserved {
		st.Evidence += "; since is the runtime's own record of the refusal"
	} else {
		st.Evidence += "; since is when this driver first observed the notice, " +
			"not when the refusal happened — the runtime's own record could not confirm it"
	}
	if q.ResetHint != "" {
		st.Evidence += " (reported reset: " + q.ResetHint + ")"
	}
	return st
}

// quotaPersisted is what this driver actually writes to the state store —
// fleet.QuotaBlock, embedded rather than nested, plus the one bit #56 needs
// that has no home on that wire type (see quotaBlockedState's comment on
// why it stays out of the wire shape).
//
// Embedded, specifically, rather than a nested `block` field: this key
// already holds a bare QuotaBlock on any instance that persisted one before
// #56, and encoding/json has no notion of a schema migration — a nested
// field would decode an old file's top-level `since`/`resetHint` into
// nothing, silently dropping an in-force block on the first restart after
// this upgrade (found and rejected in review, not deployed and then
// noticed). Embedded, `since` and `resetHint` stay exactly where an old
// file already has them; `sinceObserved` is simply absent on a file no
// version before #56 ever wrote, and decodes to its correct, honest
// default: false, "not record-confirmed" — true of every block this driver
// had ever persisted before this field existed.
type quotaPersisted struct {
	fleet.QuotaBlock
	SinceObserved bool `json:"sinceObserved,omitempty"`
}

func (d *Driver) saveQuotaLocked() {
	if d.store == nil {
		return
	}
	if d.quota == nil {
		_ = d.store.Save("quota", quotaPersisted{})
		return
	}
	_ = d.store.Save("quota", quotaPersisted{QuotaBlock: *d.quota, SinceObserved: d.quotaSinceObserved})
}

func (d *Driver) loadQuota() {
	if d.store == nil {
		return
	}
	var p quotaPersisted
	if found, err := d.store.Load("quota", &p); err == nil && found && !p.Since.IsZero() {
		block := p.QuotaBlock
		d.quota = &block
		d.quotaSinceObserved = p.SinceObserved
	}
}

// strandedRecord is what noteStranded persists: the text this driver
// delivered and could not confirm, and enough beside it (§5.4) to tell a
// live session from one that merely recycled the same id.
type strandedRecord struct {
	Text string    `json:"text"`
	Cwd  string    `json:"cwd"`
	At   time.Time `json:"at"`

	// ComposerDigest (muster #112) fingerprints the composer's own
	// content at the moment this record was made — screenDigest of the SAME
	// text composerText() would read back, not of Text itself, because a
	// multi-line paste renders as a collapsed marker rather than the literal
	// bytes (F49) and Text/the rendered composer can legitimately differ.
	//
	// This is what makes the #112 replace path safe: a later attempt to
	// clear this composer and deliver something else may proceed ONLY when
	// the composer's CURRENT digest still matches this one, which proves
	// nothing has been typed there since this driver made this record. A
	// mismatch — or an empty digest, from a record made before this field
	// existed — degrades to the honest refusal naming `discard`, never a
	// guess. Empty on a record from before muster #112.
	ComposerDigest string `json:"composerDigest,omitempty"`

	// TranscriptPath/TranscriptOffset (#180 review fix): the
	// transcript this driver resolved for this session AT STRAND TIME, and
	// the byte offset a match must come after — the identical pair
	// resolveTranscriptSource produces for an ordinary send, captured here so
	// a LATER resumeIfStranded call that finds the composer already EMPTY
	// (Send's own empty-composer check) can check whether the runtime
	// accepted this text sometime between the strand and the resume, instead
	// of blindly pasting it again — see noteStrandedWithTranscript's own doc
	// comment. Empty when no transcript could be resolved at strand time
	// (the ordinary case on a fresh session/`/clear`, when
	// WithProcessSessionsRoot/WithRecordRoot are not configured, or for a
	// record noted through the plain noteStranded — every existing caller of
	// that keeps working exactly as before, just without this corroboration).
	TranscriptPath   string `json:"transcriptPath,omitempty"`
	TranscriptOffset int64  `json:"transcriptOffset,omitempty"`

	// PasteIndex/PasteLines/PasteLanded (#180 H1): the collapsed-paste
	// marker this delivery was attributed by, when it landed as one. A long
	// or many-line paste renders as "[Pasted text #N +L lines]", which no
	// text comparison can read back; the marker this driver itself saw
	// appear is what lets a resume recognise its own strand.
	PasteIndex  int  `json:"pasteIndex,omitempty"`
	PasteLines  int  `json:"pasteLines,omitempty"`
	PasteLanded bool `json:"pasteLanded,omitempty"`

	// NeverRendered (#180 M1): the delivery stranded because nothing it
	// pasted ever showed in the composer, and no Enter was pressed for it.
	// Only this class may be pasted again by a resume that finds the
	// composer empty: with no Enter, the runtime cannot have taken it as a
	// turn unless its own transcript says so, and a resume checks that
	// first.
	NeverRendered bool `json:"neverRendered,omitempty"`
}

func (r strandedRecord) pasteKey() pasteKey {
	return pasteKey{index: r.PasteIndex, lines: r.PasteLines}
}

// strandedFile is the durable document, one entry per session with a
// delivery still unfinished. Its own file (state.go: one JSON document per
// concern), never folded into idempotency.json — a create key and a
// delivery-in-progress are different concerns with different shapes.
type strandedFile struct {
	Records    map[string]strandedRecord      `json:"records"`
	Tombstones map[string][]strandedTombstone `json:"tombstones,omitempty"`
	// Unconfirmed is #184's cross-path ledger. A file written before it existed
	// decodes with it empty, and an older build ignores the key.
	Unconfirmed map[string][]unconfirmedEntry `json:"unconfirmed,omitempty"`
}

const strandedFileName = "stranded"

// noteStranded records text this driver delivered and could not confirm.
//
// The record is what a resume corroborates against. It is deliberately OUR
// account of what we did, not a reading of the screen: the screen cannot show
// a long message back (F49), and composer contents are not evidence anyone
// meant to send them.
//
// cwd travels with it (#11): once this record survives a restart, an id
// match alone is the exact thing §5.4 forbids trusting for a
// resuming-or-destructive operation, and this is both.
//
// composerDigest is screenDigest of the composer's CURRENT rendered content
// at the moment of this call — see strandedRecord.ComposerDigest for why
// this is not simply screenDigest(text).
func (d *Driver) noteStranded(id, cwd, text, composerDigest string) {
	d.noteStrandedWithTranscript(id, cwd, text, composerDigest, transcriptSource{}, false)
}

// noteStrandedWithTranscript is noteStranded plus the #180 review
// fix: it also records WHERE this driver would look, and from what offset,
// to find out later whether the runtime accepted this exact text — src/srcOK
// is resolveTranscriptSource's own return, resolved by the caller at the
// moment this delivery stranded. A LATER resumeIfStranded call that finds
// the composer already empty (see Send's own empty-composer check, above the
// composerFound-and-busy branch) uses exactly this pair to tell "the runtime
// already accepted it" apart from "nothing confirms it either way", instead
// of falling through to a fresh paste that could duplicate an already-
// accepted delivery. srcOK=false (no transcript configured, or none could be
// resolved at strand time) leaves TranscriptPath empty — the same honest
// degrade every other caller of resolveTranscriptSource already makes.
func (d *Driver) noteStrandedWithTranscript(id, cwd, text, composerDigest string, src transcriptSource, srcOK bool) {
	d.noteStrandedLanding(id, cwd, text, composerDigest, src, srcOK, landing{}, false)
}

// noteStrandedLanding is noteStrandedWithTranscript plus how the text was
// confirmed landed, when it was: a delivery attributed by its collapsed-paste
// marker keeps that marker, so a resume can finish it (#180 H1).
func (d *Driver) noteStrandedLanding(id, cwd, text, composerDigest string, src transcriptSource, srcOK bool, l landing, neverRendered bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stranded == nil {
		d.stranded = map[string]strandedRecord{}
	}
	if prior, ok := d.stranded[id]; ok && (prior.Text != text || prior.Cwd != cwd) {
		d.buryLocked(id, prior)
	}
	rec := strandedRecord{Text: text, Cwd: cwd, At: d.now(), ComposerDigest: composerDigest}
	if l.ok && l.by == landedByMarker {
		rec.PasteIndex, rec.PasteLines, rec.PasteLanded = l.key.index, l.key.lines, true
	}
	rec.NeverRendered = neverRendered
	if srcOK {
		rec.TranscriptPath = src.path
		rec.TranscriptOffset = src.offset
	}
	d.stranded[id] = rec
	d.saveStrandedLocked()
}

// noteUnexplainedLabelled counts an unsent composer that opens with the sender
// label this driver writes on the terminal path and that nothing the driver
// remembers explains (#240). Diagnostic only — a person can type the same
// opening — and never a reason to touch the composer. A nonzero rate after
// #240 means a hop other than the queue hand-back leaves a labelled delivery
// unrecorded; the log line says whether a record exists under a different
// working directory, the one other way a record is hidden from this lookup.
func (d *Driver) noteUnexplainedLabelled(id, cwd, pending string) {
	if !strings.HasPrefix(strings.TrimSpace(pending), panePrefix) {
		return
	}
	d.counters.incr(counterStrandedUnexplainedLabelledInput)
	d.mu.Lock()
	rec, hasRec := d.stranded[id]
	tombs := len(d.tombstones[id])
	d.mu.Unlock()
	log.Printf("tmux: session %q holds unsent text opening with a sender label that no record explains "+
		"(live record for the id: %v, under a different working directory: %v; tombstones for the id: %d)",
		id, hasRec, hasRec && rec.Cwd != cwd, tombs)
}

// strandedRecordFor reports the stranded record for this session, if a live
// one exists — the same corroboration and retention discipline
// strandedMatches applies (§5.4: id + cwd, not id alone; strandedRetention),
// without requiring the text to match. strandedMatches itself is kept
// exactly as it was for the resume path (muster #112's plan: reuse the
// existing exact-match call there rather than re-deriving its equality from
// this accessor); this is for the three cases the resume path does not
// cover — same text without ResumeIfStranded, different text, and (from the
// replace path) whatever text is there now.
func (d *Driver) strandedRecordFor(id, cwd string) (strandedRecord, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepStrandedLocked()
	prior, ok := d.stranded[id]
	if !ok || prior.Cwd != cwd {
		return strandedRecord{}, false
	}
	return prior, true
}

// currentComposerDigest re-captures one pane and fingerprints whatever its
// composer currently holds — screenDigest of the same text composerText()
// would read back, the digest strandedRecord.ComposerDigest and #112's
// replace path both compare against. Empty when the capture fails or the
// composer reads empty; a caller treats that the same way an absent
// ComposerDigest is already treated elsewhere — degrade to the honest
// answer, never guess.
func (d *Driver) currentComposerDigest(ctx context.Context, paneID string) string {
	sc, ok := d.captureForClassify(ctx, paneID)
	if !ok {
		return ""
	}
	// composerClipped degrades to "" here exactly like composerAbsent
	// already did: this is documented as an honest degrade a caller already
	// treats the same as an absent ComposerDigest elsewhere (see this
	// function's own doc comment) — muster#134 does not change that,
	// because a caller with no digest to quote already falls back to the
	// screen-scope corroboration keys.go uses instead.
	pending, scan := composerText(sc)
	if scan != composerFound || pending == "" {
		return ""
	}
	return composerTextDigest(pending)
}

// tryReplaceStranded is muster #112's opt-in door out of the busy-
// composer refusal: clear a composer this driver's own record says IT
// stranded, then let the caller's ORIGINAL Send fall through to deliver
// different text in its place.
//
// Safety rests entirely on record.ComposerDigest matching the composer's
// CURRENT content (pending, already read by the caller before this is
// called) — proof that nothing has been typed there since this driver made
// the record. A record with no digest (predates muster #112) or a
// digest that no longer matches is degraded to an honest refusal naming
// `discard`, never guessed past; see driver.SendOptions.ReplaceIfStranded
// for why this can never be inferred from anything less.
//
// cleared=true means the composer is now empty and the stranded record has
// already been forgotten — the caller falls through to the ordinary
// delivery path with the NEW text. cleared=false means the returned receipt
// IS Send's answer, unchanged. err is non-nil only for a failed multiplexer
// call, never for an honest "did not clear" (that is receipt, not err).
//
// expectedLines is composerVisualLines' count for the same screen pending
// was read from — the caller already has that screen, see clearComposer's
// own doc comment for why this is what a press budget is sized to now
// (muster#129). sc is that SAME screen, passed through so clearComposer
// can tell whether the row it is about to press against is blank
// (muster#132) — see clearComposer's own doc comment for why.
func (d *Driver) tryReplaceStranded(ctx context.Context, ref fleet.SessionRef, target *paneRow, record strandedRecord, pending string, expectedLines int, sc screen) (receipt fleet.DeliveryReceipt, cleared bool, err error) {
	digest := composerTextDigest(pending)

	// #87: refuse outright, before pressing anything, if a full,
	// content-sized pass against this EXACT residue already proved it will
	// not move — the same discipline Discard itself applies before ever
	// touching the pane, and Discard's own doc comment is where the #129
	// reasoning for why this still holds is recorded.
	if attempts := d.futileClearAttempts(ref.ID, target.cwd, digest); attempts > 0 {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "a previous attempt to clear this exact composer residue made no " +
				"progress at all (#87); refusing to press the same keys into a composer " +
				"already proven not to move. discard the session directly instead " +
				"(discard?expect=" + digest + "), or wait for the residue to change",
		}, false, nil
	}

	// The draft rule's proof — the record or the caller's expect digest —
	// was established by the caller (Send) against this same read.

	left, moved, didClear, err := d.clearComposer(ctx, target.paneID, ref.ID, target.cwd, pending, expectedLines, sc)
	if err != nil {
		return fleet.DeliveryReceipt{}, false, err
	}
	if !didClear {
		reason := "attempted to clear this driver's own stranded delivery to make room " +
			"for the replacement text, but the composer did not fully empty"
		if !moved {
			reason += " and made no progress at all — a further replaceIfStranded attempt " +
				"is refused (#87) until the residue changes; discard the session directly " +
				"instead"
		} else {
			reason += fmt.Sprintf("; %d character(s) remain. The stranded record is kept, "+
				"but its digest no longer matches this composer, so a retry will be told to "+
				"discard first rather than clear again — read the session and discard the "+
				"composer directly", len(left))
		}
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, false, nil
	}

	d.forgetStranded(ref.ID)
	return fleet.DeliveryReceipt{}, true, nil
}

// tryClearUnrecordedComposer is muster #135's door out of the §2.4
// refusal for the shape tryReplaceStranded does not cover: a composer
// holding text this driver has NO stranded record of at all — the ordinary
// reason resumeIfStranded/replaceIfStranded land here in the first place
// (Send's own else-branch just above). Neither flag has anything to
// literally "resume" without a record of what was sent, so both take the
// same door out: clear whatever the composer holds right now and let the
// caller's Send fall through to deliver THIS call's text — exactly what a
// caller was previously forced to do by hand across three round trips
// (read → discard?expect=<digest> → resend), and exactly the gap #135's
// field report described.
//
// # Safety is relocated, not loosened
//
// Corroboration is the digest of `pending` — the SAME composer read Send
// already performed moments before calling this, not a fresh capture — so
// this offers no less protection against a human typing in between "look"
// and "clear" than /discard's own `?expect=` does; it supplies the "look"
// step from data already in hand instead of demanding a separate round trip
// the caller's opt-in flag already declared redundant. What replaces
// /discard's requirement that a CALLER have separately read the composer
// before destroying it is the opt-in flag on this very Send call: setting
// resumeIfStranded or replaceIfStranded IS the caller's considered decision
// to clear whatever is there, the same way setting either flag against a
// recognised stranded record already is (tryReplaceStranded, above). A bare
// /input with neither flag set never reaches this function.
//
// # No new stranded record on failure
//
// This driver never delivered anything of its own into this composer, so a
// failed or partial clear leaves nothing of "ours" to keep a record of —
// unlike tryReplaceStranded, which keeps the PRE-EXISTING record when its
// clear does not finish. The failure is reported honestly instead, using
// discardIncomplete's own two-shape distinction (unchanged vs
// changed-but-nonempty) inline rather than the pre-formed error, because
// this returns a DeliveryReceipt, not an error.
//
// cleared=true means the composer is now empty; the caller falls through to
// the ordinary delivery path with the NEW text, the same contract
// tryReplaceStranded's own cleared=true carries. cleared=false means the
// returned receipt IS Send's answer. err is non-nil only for a failed
// multiplexer call, never for an honest "did not clear".
func (d *Driver) tryClearUnrecordedComposer(ctx context.Context, ref fleet.SessionRef, target *paneRow, pending string, expectedLines int, sc screen, opts driver.SendOptions) (receipt fleet.DeliveryReceipt, cleared bool, err error) {
	digest := composerTextDigest(pending)
	flag := "replaceIfStranded"
	if opts.ResumeIfStranded {
		flag = "resumeIfStranded"
	}

	// #87: identical discipline to tryReplaceStranded and Discard itself —
	// refuse outright, before pressing anything, if a full, content-sized
	// pass against this EXACT residue already proved it will not move.
	if attempts := d.futileClearAttempts(ref.ID, target.cwd, digest); attempts > 0 {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "a previous attempt to clear this exact composer residue made no " +
				"progress at all (#87); refusing to press the same keys into a composer " +
				"already proven not to move. discard the session directly instead " +
				"(discard?expect=" + digest + "), or wait for the residue to change",
		}, false, nil
	}

	left, moved, didClear, err := d.clearComposer(ctx, target.paneID, ref.ID, target.cwd, pending, expectedLines, sc)
	if err != nil {
		return fleet.DeliveryReceipt{}, false, err
	}
	if !didClear {
		reason := "this driver holds no record of having put anything in this composer, but " +
			flag + " asked to clear it and deliver this text anyway; the clear did not " +
			"fully empty the composer"
		if !moved {
			reason += " and made no progress at all — the keystroke never registered, so the " +
				"composer is unchanged from what this call read; a further " + flag +
				" attempt is refused (#87) until the residue changes — discard the session " +
				"directly instead (discard?expect=" + digest + ")"
		} else {
			reason += fmt.Sprintf("; %d character(s) remain (found digest %s) — the composer "+
				"now holds neither the original text nor nothing, so it is damaged, not "+
				"merely unclear. Read the session before doing anything else rather than "+
				"retrying blind", len(left), screenDigest(left))
		}
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, false, nil
	}

	return fleet.DeliveryReceipt{}, true, nil
}

// strandedMatches reports whether text is exactly what this driver left in
// that session's composer, in the session the record was made for.
//
// Exact text, not prefix: "resume the delivery I made" is a different
// request from "submit something that starts the same way", and only the
// first is the one the caller is owed. cwd is required too (§5.4) — a
// durable record can outlive the session it describes, and an id is
// recyclable; matching on id and text alone would resume into whatever
// unrelated session later reused that name. A record older than
// strandedRetention is treated as absent, the same reasoning
// idemStore.sweepLocked applies to an expired idempotency key: kept
// forever, it stops being evidence about anything in particular.
func (d *Driver) strandedMatches(id, cwd, text string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepStrandedLocked()
	prior, ok := d.stranded[id]
	return ok && prior.Text == text && prior.Cwd == cwd
}

func (d *Driver) forgetStranded(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.stranded, id)
	d.saveStrandedLocked()
}

// rekeySessionState moves this driver's per-id memory of a session from an
// OLD id to a NEW one after Rename's multiplexer-level rename has already
// succeeded — muster #223, the same principle d.observed (Rename, above)
// and d.mods.rekey (#185) already apply, extended to the four records #223
// found were left behind: a stranded delivery, its tombstones, the #184
// cross-path ledger, and the delivery mark #111's `turns` is counted from.
// Left keyed to the OLD id, each of these is orphaned by a rename — a resume
// against the NEW id would not find a stranded record still sitting under
// the id that no longer exists, and the same blindness applies to every
// other reader of these four maps.
//
// The two singular records (stranded, delivery mark) are moved the same way
// d.mods.rekey moves a lane: unconditionally overwriting whatever the new id
// already holds. That mirrors d.observed's own move a few lines above in
// Rename, and a live entry under the NEW id is not expected — Rename's
// caller has just been refused a moment ago if `to` named a currently-live
// session, so anything found here is, at most, a stale leftover of a past
// session that once carried this exact id.
//
// The two list-shaped records (tombstones, unconfirmed ledger) are MERGED
// instead of overwritten, because unlike the singular records they are
// evidence about several distinct past deliveries, each self-corroborated by
// its own Cwd field (§5.4) rather than by the id alone — dropping the new
// id's own list to make room for the old one's would silently reopen the
// double-delivery and draft-misattribution gaps those two ledgers exist to
// close, for whichever session last held the new id.
func (d *Driver) rekeySessionState(from, to string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if rec, ok := d.stranded[from]; ok {
		delete(d.stranded, from)
		d.stranded[to] = rec
	}
	if list, ok := d.tombstones[from]; ok {
		delete(d.tombstones, from)
		merged := append(append([]strandedTombstone(nil), d.tombstones[to]...), list...)
		if len(merged) > tombstonesPerSession {
			merged = merged[len(merged)-tombstonesPerSession:]
		}
		d.tombstones[to] = merged
	}
	if list, ok := d.unconfirmed[from]; ok {
		delete(d.unconfirmed, from)
		merged := append(append([]unconfirmedEntry(nil), d.unconfirmed[to]...), list...)
		if len(merged) > unconfirmedPerSession {
			merged = merged[len(merged)-unconfirmedPerSession:]
		}
		d.unconfirmed[to] = merged
	}
	d.saveStrandedLocked()

	if rec, ok := d.delivered[from]; ok {
		delete(d.delivered, from)
		d.delivered[to] = rec
	}
	d.saveDeliveryLocked()
}

// sweepStrandedLocked drops records older than strandedRetention. Caller
// holds d.mu. Cheap and unconditional: the map is at most one entry per
// session with a delivery genuinely in flight, never a growing log.
func (d *Driver) sweepStrandedLocked() {
	if len(d.stranded) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.stranded {
		if now.Sub(rec.At) > strandedRetention {
			// #180 M2: the record lapses, the proof that this text is the
			// driver's own does not — see draftrule.go.
			d.buryLocked(id, rec)
			delete(d.stranded, id)
		}
	}
}

// futileClear is what noteFutile records: a composer residue Discard's
// clear loop already spent one full pass on and could not move (#87).
//
// Cwd travels with it for the same reason it does on strandedRecord (§5.4):
// an id is recyclable, and matching on id alone would let a record made for
// one session's composer be misread as describing an unrelated session
// that later reused the same id. ResidueDigest is what makes the match
// exact rather than merely "this id had trouble once" — a caller's own
// progress (a NEW residue, a new digest) must never be blocked by a record
// that describes a DIFFERENT, earlier piece of text.
type futileClear struct {
	Cwd           string
	ResidueDigest string
	Attempts      int
	At            time.Time
}

// noteFutile records that a clear pass against this exact residue produced
// no movement at all. Called only from Discard's own "unchanged" branch.
//
// Attempts counts consecutive passes against the SAME (cwd, residue) pair;
// a new residue (the composer changed, however slightly) or a different cwd
// (the id was recycled) starts back at 1 rather than accumulating, because
// neither describes the state this attempt just observed.
func (d *Driver) noteFutile(id, cwd, residueDigest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.futile == nil {
		d.futile = map[string]futileClear{}
	}
	attempts := 1
	if prior, ok := d.futile[id]; ok && prior.Cwd == cwd && prior.ResidueDigest == residueDigest {
		attempts = prior.Attempts + 1
	}
	d.futile[id] = futileClear{Cwd: cwd, ResidueDigest: residueDigest, Attempts: attempts, At: d.now()}
}

// futileClearAttempts reports how many consecutive times a pass against
// this EXACT residue has already produced zero movement — 0 means no
// matching record, i.e. Discard has not yet spent a pass on this text.
//
// Corroborated on cwd, not id alone (§5.4, same rule strandedMatches
// applies): a record for a recycled id describes a different session's
// composer and must not gate this one. A record older than
// futileClearRetention is treated as absent, the same reasoning
// sweepStrandedLocked already applies to stranded records.
func (d *Driver) futileClearAttempts(id, cwd, residueDigest string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepFutileLocked()
	prior, ok := d.futile[id]
	if !ok || prior.Cwd != cwd || prior.ResidueDigest != residueDigest {
		return 0
	}
	return prior.Attempts
}

// forgetFutile drops any futile-clear record for id. Called once Discard
// actually empties the composer — evidence that whatever was stopping a
// prior pass no longer applies, so a future stall against a NEW residue
// deserves a fresh full pass, not a record left over from a different one.
func (d *Driver) forgetFutile(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.futile, id)
}

// sweepFutileLocked drops records older than futileClearRetention. Caller
// holds d.mu. Cheap and unconditional, same shape as sweepStrandedLocked:
// at most one entry per session with a clear pass genuinely still stuck.
func (d *Driver) sweepFutileLocked() {
	if len(d.futile) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.futile {
		if now.Sub(rec.At) > futileClearRetention {
			delete(d.futile, id)
		}
	}
}

func (d *Driver) saveStrandedLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(strandedFileName, strandedFile{Records: d.stranded, Tombstones: d.tombstones, Unconfirmed: d.unconfirmed})
}

// loadStranded restores stranded-delivery records at startup, sweeping
// anything already past strandedRetention — the same "sweep on load" shape
// idemStore uses, so a record that expired while the service was down does
// not get a fresh window just for having survived to be read.
func (d *Driver) loadStranded() {
	if d.store == nil {
		return
	}
	var f strandedFile
	found, err := d.store.Load(strandedFileName, &f)
	if err != nil || !found || (len(f.Records) == 0 && len(f.Tombstones) == 0 && len(f.Unconfirmed) == 0) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stranded = f.Records
	d.tombstones = f.Tombstones
	d.unconfirmed = f.Unconfirmed
	d.sweepTombstonesLocked()
	d.sweepStrandedLocked()
	d.sweepUnconfirmedLocked()
	d.saveStrandedLocked()
}

// deliveryMark is what noteDelivery persists: the moment of this driver's
// most recent delivery into one session's composer, and a memo of the last
// turn count successfully computed from it (muster #111).
type deliveryMark struct {
	// Cwd corroborates the same way every other durable record here does
	// (§5.4) — an id alone is recyclable.
	Cwd string `json:"cwd"`
	// At is the delivery this mark's count is "since". Never moved by a
	// resume finishing the SAME delivery — only a fresh paste starts a new
	// mark.
	At time.Time `json:"at"`

	// Count and Size are a memo-and-latch pair, not raw state: Count is the
	// last successfully computed turn count, Size is the runtime record's
	// file size at the moment that count was computed. A later read whose
	// record size is UNCHANGED reuses Count instead of re-parsing up to
	// 256KiB; a later read that cannot resolve the record at all (a
	// transient stat/open failure) reports this Count rather than flapping
	// `turns` to absent for a reason that has nothing to do with whether the
	// session is alive. Zero Size means no count has ever been computed for
	// this mark yet — the honest starting state, not "zero turns".
	Count int   `json:"count"`
	Size  int64 `json:"size"`
}

// deliveryFile is the durable document — one entry per session with a
// remembered delivery. Its own file, same reasoning strandedFile already
// gives: a create key, a stranded delivery and a delivery mark are three
// different concerns with three different shapes and three different
// lifetimes.
type deliveryFile struct {
	Records map[string]deliveryMark `json:"records"`
}

const deliveryFileName = "delivery-mark"

// noteDelivery records that this driver just pasted text into a session's
// composer — see the field's own doc comment on Driver.delivered for why
// this is the single write site every downstream outcome shares, and why a
// resume must never call this.
func (d *Driver) noteDelivery(id, cwd string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delivered == nil {
		d.delivered = map[string]deliveryMark{}
	}
	d.delivered[id] = deliveryMark{Cwd: cwd, At: d.now()}
	d.saveDeliveryLocked()
}

// deliveryMarkFor reports the live delivery mark for a session, if any —
// the same id+cwd corroboration and retention discipline as
// strandedRecordFor, on deliveryMarkRetention's longer clock.
func (d *Driver) deliveryMarkFor(id, cwd string) (deliveryMark, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepDeliveryLocked()
	prior, ok := d.delivered[id]
	if !ok || prior.Cwd != cwd {
		return deliveryMark{}, false
	}
	return prior, true
}

// updateDeliveryMarkCount refreshes a mark's memoised count after a
// successful turnsSince read — see deliveryMark.Count/Size. A no-op if the
// mark has since been forgotten or replaced by a new delivery (its Cwd or At
// would then no longer match what the caller resolved the count against);
// silently doing nothing in that case is correct, not a bug swallowed — the
// caller's OWN read already has its answer, this only refreshes the cache
// for the NEXT one.
func (d *Driver) updateDeliveryMarkCount(id, cwd string, at time.Time, count int, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prior, ok := d.delivered[id]
	if !ok || prior.Cwd != cwd || !prior.At.Equal(at) {
		return
	}
	prior.Count = count
	prior.Size = size
	d.delivered[id] = prior
	d.saveDeliveryLocked()
}

// forgetDelivery drops a session's delivery mark. Called from Close (#111,
// mirroring forgetStranded): a destroyed session's `turns` denominator is
// gone with it, the same way its composer is.
func (d *Driver) forgetDelivery(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.delivered, id)
	d.saveDeliveryLocked()
}

// sweepDeliveryLocked drops marks older than deliveryMarkRetention. Caller
// holds d.mu. Same shape as sweepStrandedLocked, longer clock.
func (d *Driver) sweepDeliveryLocked() {
	if len(d.delivered) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.delivered {
		if now.Sub(rec.At) > deliveryMarkRetention {
			delete(d.delivered, id)
		}
	}
}

func (d *Driver) saveDeliveryLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(deliveryFileName, deliveryFile{Records: d.delivered})
}

// loadDelivery restores delivery marks at startup, sweeping anything already
// past deliveryMarkRetention — same "sweep on load" shape as loadStranded.
func (d *Driver) loadDelivery() {
	if d.store == nil {
		return
	}
	var f deliveryFile
	found, err := d.store.Load(deliveryFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivered = f.Records
	d.sweepDeliveryLocked()
	d.saveDeliveryLocked()
}

// stampSinceLocked fills §2.3's Since: when this status was FIRST observed to
// hold, not when it began. Caller holds d.mu.
//
// For a session holding unsent input the evidence also gains the age, because
// that is the number a human needs and the one that distinguishes "somebody is
// typing" from "nobody is ever coming back". A caller reading `since` can
// compute it; a caller reading a log line cannot.
// attachHint describes how a person gets a terminal onto this session (§2.8).
//
// The binary path is this machine's, resolved the same way every other
// invocation resolves it — which matters, because the reason FLEET_TMUX_BIN
// exists is that a non-interactive shell on these machines does not have the
// multiplexer on PATH. A hint containing a bare "tmux" would work when tested
// interactively and fail exactly where a supervisor would run it.
//
// No remote form is produced. This driver knows the machine it runs on; it
// does not know how a caller reaches that machine, and inventing an ssh line
// would be this service asserting a network topology it cannot see (§7.2).
func (d *Driver) attachHint(session string) *fleet.AttachHint {
	bin := d.attachBin()
	return &fleet.AttachHint{
		Kind:   "multiplexer",
		Target: session,
		// -t takes the name verbatim; ids here routinely contain emoji and
		// spaces, which is why this is argv and not a command string.
		Command: []string{bin, "attach-session", "-t", session},
		// -r attaches read-only: the viewer sees the session and cannot type
		// into it. This is the one a supervisor should offer for "watch",
		// because the read-write attachment shares a real keyboard with
		// whatever the agent is doing.
		ReadOnly: []string{bin, "attach-session", "-r", "-t", session},
		// The multiplexer permits many concurrent clients on one session, so
		// attaching never evicts anyone.
		Shared: true,
	}
}

// attachBin resolves the multiplexer to an absolute path for the hint.
//
// The driver itself can run a bare "tmux" because whatever PATH it inherited
// resolved it. A hint is executed somewhere else entirely — possibly by a
// supervisor's non-interactive shell, which on these machines gets a bare PATH
// that does not include the package manager's prefix. Handing out a name that
// works here and not there would produce a hint that fails only in production,
// which is the same trap FLEET_TMUX_BIN exists for.
//
// Falls back to the configured value when resolution fails: a name is a worse
// answer than a path, and still better than nothing.
func (d *Driver) attachBin() string {
	if filepath.IsAbs(d.bin) {
		return d.bin
	}
	if resolved, err := exec.LookPath(d.bin); err == nil {
		return resolved
	}
	return d.bin
}

// memoryLocked returns what the driver remembers of a pane's last screen.
// Caller holds d.mu.
func (d *Driver) memoryLocked(id string) paneMemory {
	prior, ok := d.observed[id]
	if !ok || prior.digest == "" {
		return paneMemory{}
	}
	at := prior.digestSince
	if at.IsZero() {
		at = prior.at
	}
	return paneMemory{known: true, digest: prior.digest, at: at}
}

// digestSinceLocked returns when id's pane was first seen showing digest:
// the remembered time if the previous observation already showed it, or now
// if this read is the first to see it (#159 — see observation.digestSince).
// Call it BEFORE overwriting d.observed[id]. Caller holds d.mu.
func (d *Driver) digestSinceLocked(id, digest string, now time.Time) time.Time {
	if prior, ok := d.observed[id]; ok && digest != "" && prior.digest == digest {
		if !prior.digestSince.IsZero() {
			return prior.digestSince
		}
		return prior.at
	}
	return now
}

// restoredWaitingInputSince reports whether the CURRENT waiting_input status
// for id is known — either from this instance's own in-memory observation,
// or from a record persisted to disk before this instance started — to
// predate this service's current process, and since when.
//
// This is the exact "restored" fact stampSinceLocked already computes for a
// State/List read's Evidence line (see that function, immediately below):
// muster #124's own field report quoted that evidence verbatim —
// "unchanged for 49m0s (age carried from before this service restarted)".
// What #124 found missing is that Discard's OWN failure messages
// (discardIncomplete, discardProvenFutile) never carried this same fact, so
// an operator seeing a 409 had to separately call State(), notice the
// phrase, and do the correlation by hand — which is exactly what #124's
// report spent two follow-up comments doing. This is factored out as its
// own method (rather than inlined at Discard's call sites) so it reads the
// SAME two sources stampSinceLocked reads, in the same order, and can never
// silently drift into a second, slightly different definition of
// "restored".
//
// Locks d.mu itself — unlike stampSinceLocked, which assumes the caller
// already holds it (it runs inside State/List's own locked section).
// Discard does not hold d.mu at its call site, so this manages its own.
func (d *Driver) restoredWaitingInputSince(id string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if prior, ok := d.observed[id]; ok && prior.status == fleet.StatusWaitingInput {
		return prior.statusSince, prior.sinceRestored
	}
	if rec, ok := d.persistedRecord(id); ok &&
		rec.Status == string(fleet.StatusWaitingInput) && !rec.StatusSince.IsZero() {
		return rec.StatusSince, true
	}
	return time.Time{}, false
}

// restartNote reports restoredWaitingInputSince's fact as a ready-to-append
// clause, and ok=false when it does not apply to id.
//
// The phrase deliberately reuses stampSinceLocked's own wording ("carried
// from before this service restarted") verbatim rather than inventing new
// prose, so an operator who has already seen that phrase on a State() read
// recognizes it immediately here.
//
// Factored out of what used to be withRestartNote's own body so muster
// #131 can reuse the fact on Send's OutcomeUnknown receipts (see
// withRestartNoteReason) without also carrying Discard's remedy clause below
// — that clause is specific to a stuck composer C-u cannot move; Send's own
// Reason strings already say how to retry, and appending Discard's advice
// there would be wrong for what the caller is looking at.
func (d *Driver) restartNote(id string) (string, bool) {
	since, restored := d.restoredWaitingInputSince(id)
	if !restored {
		return "", false
	}
	return fmt.Sprintf("this composer's unsent-input status was already "+
		"holding before this service's current process started (age carried "+
		"from before this service restarted, since %s)",
		since.Format(time.RFC3339)), true
}

// withRestartNote appends restartNote's fact, plus Discard's own remedy
// clause, to err's message when it applies to id, and returns err unchanged
// otherwise (including when err is nil). Wrapped with %w so
// errors.Is(_, ErrAmbiguousTarget) still holds on the result — every caller
// of Discard's error checks that kind, and appending a note by plain string
// concatenation instead would silently break that check for exactly the
// sessions this note is meant to help.
func (d *Driver) withRestartNote(id string, err error) error {
	if err == nil {
		return err
	}
	note, ok := d.restartNote(id)
	if !ok {
		return err
	}
	return fmt.Errorf("%w; %s — closing the session is the one remedy known "+
		"to work against a residue in that condition", err, note)
}

// withRestartNoteReason appends restartNote's fact to reason when it applies
// to id, and returns reason unchanged otherwise. muster #131: the same
// correlation Discard's 409s carry via withRestartNote, reused on Send's own
// OutcomeUnknown receipts — a caller retrying a swallowed submit or an
// unconfirmed paste should not have to cross-reference a separate State()
// read to learn the session's waiting_input status predates this service's
// current process. Unlike withRestartNote this never wraps an error — every
// Send call site below already returns (fleet.DeliveryReceipt, nil) on this
// path, so there is no error kind to preserve.
func (d *Driver) withRestartNoteReason(id, reason string) string {
	note, ok := d.restartNote(id)
	if !ok {
		return reason
	}
	return reason + "; " + note
}

// Returns the state and whether its `since` was carried from a previous
// instance — the caller must store that on the observation, or the provenance
// is lost the moment the value is cached and every later read presents a
// second-hand age as one this instance measured.
//
// captured is false when the read observed nothing (a driver malfunction, not
// a status). Such a read is stamped from the start of the gap, and never
// touches the last real observation — see recordObservationLocked (#278).
func (d *Driver) stampSinceLocked(id string, st fleet.SessionState, now time.Time, captured bool) (fleet.SessionState, bool) {
	since := now
	restored := false
	if prior, ok := d.observed[id]; !captured && ok && !prior.statusSince.IsZero() {
		// The unknown status began when the reads started failing, not now:
		// a gap that lasts ten polls is one stretch of unknown.
		if !prior.gapSince.IsZero() {
			since = prior.gapSince
		}
		st.Since = &since
		return st, false
	}
	if prior, ok := d.observed[id]; ok && prior.status == st.Status && !prior.statusSince.IsZero() {
		since = prior.statusSince
		restored = prior.sinceRestored
	} else if rec, ok := d.persistedRecord(id); ok &&
		rec.Status == string(st.Status) && !rec.StatusSince.IsZero() {
		// No in-memory sighting, but the same status was recorded before this
		// instance started. Carrying it is the difference between reporting a
		// 14-hour stall and reporting the service's own uptime.
		since = rec.StatusSince
		restored = true
	}
	st.Since = &since

	if st.Status == fleet.StatusWaitingInput && strings.Contains(st.Evidence, "unsent input") {
		if age := now.Sub(since); age > unsentAgeWorthMentioning {
			st.Evidence += "; unchanged for " + age.Round(time.Minute).String()
		}
	}
	// Say where the number came from. §5.2 forbids presenting inference as
	// observation, and an age this instance did not measure is exactly that —
	// the value is worth keeping, the provenance is not optional.
	if restored {
		st.Evidence += " (age carried from before this service restarted)"
	}
	return st, restored
}

// recordObservationLocked stores what a read saw of a pane. Caller holds d.mu.
//
// A read that captured nothing (captured == false) is a driver malfunction, and
// its own classification says so: not an observation about the session. If an
// earlier real observation exists it is KEPT as it is, with only gapSince
// marked, so that when the next good read finds the same status the `since`
// carries across the gap instead of restarting at now (#278).
//
// This covers every cause alike: a chunk-wide failure and a partial one (some
// captures in an invocation missing) both arrive here as captured == false for
// the panes that came back empty, and the sessions that did capture in the same
// invocation are untouched by it.
//
// With no earlier observation there is nothing to protect, and the unknown read
// is stored as a first sighting, as it always was.
func (d *Driver) recordObservationLocked(id string, o observation, captured bool, since time.Time) {
	if !captured {
		if prior, ok := d.observed[id]; ok && !prior.statusSince.IsZero() {
			prior.gapSince = since
			d.observed[id] = prior
			return
		}
	}
	d.observed[id] = o
}

// persistedRecord reads one session's durable record. Caller holds d.mu.
func (d *Driver) persistedRecord(id string) (sessionRecord, bool) {
	if d.store == nil {
		return sessionRecord{}, false
	}
	rec, ok := d.loadRecords()[id]
	return rec, ok
}

// walkHighlight moves an unnumbered menu's highlight from its current row to
// choice with arrow keys, and reports whether a fresh read shows it there
// (muster#171). It never confirms anything itself.
//
// Arrival is read, not assumed from the key count: a press the menu swallows
// leaves the highlight short of the chosen row, and confirming then would
// answer with a row nobody chose. The prompt's nonce is checked on every
// read too — it covers the question and options but not the highlight, so
// it holds still while the highlight moves and changes only if the question
// itself was replaced mid-walk.
//
// The arrows go in ONE send-keys call, and that was measured, not assumed
// (muster#205): the runtime's unnumbered trust menu and a numbered
// picker of eight rows applied every press of a burst of up to seven. A burst
// does lose presses elsewhere — beside a preview pane (#204), and on a list
// whose first press moves focus to another control — so this is not a rule
// for other layouts: measure the layout, or send one key per call as
// answerPreview does. Should a menu ever keep only some of the arrows, the
// read-back below turns it into `unknown` with nothing confirmed.
func (d *Driver) walkHighlight(ctx context.Context, paneID string, before *fleet.SessionPrompt, choice int) (bool, string, error) {
	key, n := "Down", choice-before.Selected
	if n < 0 {
		key, n = "Up", -n
	}
	args := []string{"send-keys", "-t", paneID}
	for i := 0; i < n; i++ {
		args = append(args, key)
	}
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return false, "", err
	}
	target := "option " + strconv.Itoa(choice) + " (" + before.Options[choice-1] + ")"
	verdict, _ := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
		switch {
		case now == nil || now.Nonce != before.Nonce:
			return promptDiverged
		case now.Selected == choice:
			return promptArrived
		}
		return promptPending
	})
	switch verdict {
	case promptArrived:
		return true, "", nil
	case promptDiverged:
		return false, "pressed " + key + " to move the highlight toward " + target +
			" on a menu without numbers, and the prompt changed before anything " +
			"was confirmed; no confirm key was sent", nil
	}
	return false, "pressed " + key + " " + strconv.Itoa(n) + " time(s) to reach " + target +
		" on a menu without numbers, but the highlight did not arrive there; no " +
		"confirm key was sent, so the prompt is still up and unanswered, with the " +
		"highlight possibly moved", nil
}

// promptVerdict is what one read of the pane says about a key just sent.
type promptVerdict int

const (
	// promptPending: not there yet — read again until the window closes.
	promptPending promptVerdict = iota
	// promptArrived: the screen shows what the key was sent to produce.
	promptArrived
	// promptDiverged: the screen shows something else entirely; waiting
	// longer cannot turn it into the expected result.
	promptDiverged
)

// awaitPrompt re-reads the pane until judge says the key it follows has
// either landed or gone somewhere else, or promptClearWindow runs out
// (reported as promptPending, with the last prompt read). It is the one
// read-after-keypress loop the respond paths share, so every key they send is
// corroborated the same way rather than each path growing its own timing.
func (d *Driver) awaitPrompt(ctx context.Context, paneID string, judge func(*fleet.SessionPrompt) promptVerdict) (promptVerdict, *fleet.SessionPrompt) {
	deadline := d.now().Add(promptClearWindow)
	var last *fleet.SessionPrompt
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			last, _ = parsePromptMenu(sc)
			if v := judge(last); v != promptPending {
				return v, last
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return promptPending, last
		}
		select {
		case <-ctx.Done():
		case <-time.After(promptClearInterval):
		}
	}
}

// sameMultiSelectQuestion reports whether b is still the multi-select question
// a was: the same option labels with the checkboxes set aside, and the same
// question text. Tick state is what the caller is changing, so it is exactly
// what this comparison ignores.
//
// The dialog's tab bar used to ride inside the question text, so ticking the
// first box flipped this question's own tab from ☐ to ☒ and the comparison had
// to set that aside. Question no longer carries the header (muster#204),
// so there is nothing to set aside: the header is part of the nonce instead.
func sameMultiSelectQuestion(a, b *fleet.SessionPrompt) bool {
	return sameMultiSelectQuestionAround(a, b, 0)
}

// sameMultiSelectQuestionAround is sameMultiSelectQuestion with option skip
// (1-based; 0 sets none aside) left out of the comparison. It exists for the
// one row whose label the caller changes on purpose: the free-text row, whose
// label becomes the text typed into it (muster#206).
func sameMultiSelectQuestionAround(a, b *fleet.SessionPrompt, skip int) bool {
	if a == nil || b == nil || !b.MultiSelect || len(a.Options) != len(b.Options) {
		return false
	}
	for i := range a.Options {
		if i == skip-1 {
			continue
		}
		la, _, _ := checkboxLabel(a.Options[i])
		lb, _, _ := checkboxLabel(b.Options[i])
		if la != lb {
			return false
		}
	}
	return a.Question == b.Question
}

// answerMultiSelect answers a multi-select question with a set
// (muster#176): it flips exactly the boxes whose tick differs from the
// set, then moves the dialog on ONE step — to the next question, or to the
// review screen #159 recognises — and stops there. It never confirms the
// review screen; that is the caller's own choice on its own nonce.
//
// # What was measured, and what each step reuses
//
// Live, on one runtime build, on a single-question and a two-question
// dialog:
//
//   - a digit flips that box and nothing else — the highlight does not move
//     and the dialog does not advance — so it is #168's digit-alone keypress,
//     sent once per box, never as a burst (#168 measured a burst losing a
//     digit), and each flip is read back before the next is sent;
//   - Right moves to the next tab, ticks kept — and after the last question
//     that tab is the review screen, "Ready to submit your answers?" over
//     Submit answers / Cancel, the exact chrome reviewScreenPrompt matches;
//   - off the checkboxes — on the free-text row, the unnumbered Submit row
//     below it, or the chat row — the free-text field has focus: Right moves
//     its cursor and the dialog stays put, and a digit is TYPED into it
//     rather than flipping a box (found by the live end-to-end run, after
//     the unit model had missed it). Up from any of them walks back one row
//     at a time to the last checkbox. So before any other key the highlight
//     is moved onto a checkbox row, one Up at a time, each read back.
//
// Every key is followed by a read (awaitPrompt). A key that did not land
// stops the sequence there: nothing further is sent, and the receipt says
// which boxes were already flipped. Resending the same set with the fresh
// nonce is then safe, because the set names the end state, not the moves.
func (d *Driver) answerMultiSelect(ctx context.Context, target *paneRow, before *fleet.SessionPrompt, boxes int, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	paneID := target.paneID
	for _, c := range resp.Choices {
		if c > boxes {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason: "option " + strconv.Itoa(c) + " is not a checkbox on this " +
					"question; its checkboxes are options 1-" + strconv.Itoa(boxes),
			}, nil
		}
	}
	if boxes > 9 {
		// A digit is one keypress for 1-9 only; this shape has not been
		// seen with more boxes, and guessing its keys is not answering.
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "this question has more checkboxes than one digit can reach",
		}, nil
	}
	// muster#206: the caller's own words, typed into the free-text row
	// that sits directly under the boxes. Decided from the screen already
	// read, before any key is sent, like every other refusal here.
	var text string
	freeIdx := 0
	if resp.Text != nil {
		var refusal string
		if text, refusal = freeTextPayload(resp); refusal != "" {
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: refusal}, nil
		}
		if !before.FreeText {
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: notFreeTextReason(before)}, nil
		}
		freeIdx = freeTextRow(before)
	}
	want := make([]bool, boxes)
	for _, c := range resp.Choices {
		want[c-1] = true
	}
	have := promptTicks(before, boxes)

	label := func(i int) string {
		l, _, _ := checkboxLabel(before.Options[i-1])
		return strconv.Itoa(i) + " (" + l + ")"
	}
	var done []string
	progress := func() string {
		if len(done) == 0 {
			return "no box was flipped"
		}
		return "flipped " + strings.Join(done, ", ")
	}
	resend := "; nothing further was sent. Read the state again and resend the same " +
		"choices with the new nonce: the set names the end state, so the boxes " +
		"already flipped are not flipped twice"
	if resp.Text != nil {
		resend = "; nothing further was sent. Read the state again before answering: the " +
			"boxes already flipped are not flipped twice, but text that reached the free-text " +
			"row stays there, and a row that holds text is no longer recognised as free-text " +
			"(prompt.freeText), so answer that one with choices alone or cancel it"
	}
	unknown := func(what string) (fleet.DeliveryReceipt, error) {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason:  progress() + "; " + what + resend,
		}, nil
	}

	cur := before
	// same is this question with its ticks — and, once text is going in, its
	// free-text row — set aside; skip is that row, 0 while nothing is typed.
	skip := 0
	same := func(a, b *fleet.SessionPrompt) bool { return sameMultiSelectQuestionAround(a, b, skip) }

	// moveOntoBox puts the highlight on a checkbox row. Off the checkboxes the
	// free-text field has focus (measured): a digit is typed into it instead of
	// flipping a box, and Right moves its cursor instead of the dialog. Each Up
	// is read back; the bound is the rows below the boxes, plus the unnumbered
	// Submit row. ok is false when the receipt is final.
	moveOntoBox := func() (rcpt fleet.DeliveryReceipt, ok bool, err error) {
		for press := 0; cur.Selected < 1 || cur.Selected > boxes; press++ {
			if press > len(cur.Options)-boxes+1 {
				r, e := unknown("the highlight could not be moved onto a checkbox row, " +
					"where the key that moves the dialog on is not swallowed")
				return r, false, e
			}
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, "Up"); err != nil {
				return fleet.DeliveryReceipt{}, false, fmt.Errorf("respond: %w", err)
			}
			prev := cur
			verdict, now := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
				switch {
				case !same(prev, now):
					return promptDiverged
				case now.Selected != prev.Selected:
					return promptArrived
				}
				return promptPending
			})
			if verdict != promptArrived {
				r, e := unknown("the highlight did not move off a row where the key " +
					"that moves the dialog on is swallowed")
				return r, false, e
			}
			cur = now
		}
		return fleet.DeliveryReceipt{}, true, nil
	}
	// The highlight goes onto a checkbox FIRST, before any digit.
	if rcpt, ok, err := moveOntoBox(); !ok {
		return rcpt, err
	}

	for i := 1; i <= boxes; i++ {
		if want[i-1] == have[i-1] {
			continue
		}
		if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, strconv.Itoa(i)); err != nil {
			return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
		}
		prev := cur
		verdict, now := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
			if !same(prev, now) {
				return promptDiverged
			}
			got := promptTicks(now, boxes)
			old := promptTicks(prev, boxes)
			for j := range got {
				if (j == i-1) == (got[j] == old[j]) {
					return promptPending
				}
			}
			return promptArrived
		})
		switch verdict {
		case promptDiverged:
			return unknown("the question changed after option " + label(i) +
				" was pressed, so it was not confirmed flipped")
		case promptPending:
			return unknown("option " + label(i) + " did not read back as flipped")
		}
		cur = now
		done = append(done, label(i))
	}

	if resp.Text != nil {
		// Down onto the free-text row, one key per call and each read back: a
		// digit there would only TOGGLE its tick (measured), and the field
		// takes focus only when the highlight is on it. The row's own tick
		// follows the text — typing ticks it (measured) — so the boxes are
		// already at their end state before any of this.
		for press := 0; cur.Selected != freeIdx; press++ {
			if press > freeIdx {
				return unknown("the highlight could not be moved onto the free-text row")
			}
			if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, "Down"); err != nil {
				return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
			}
			prev := cur
			verdict, now := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
				switch {
				case !same(prev, now):
					return promptDiverged
				case now.Selected != prev.Selected:
					return promptArrived
				}
				return promptPending
			})
			if verdict != promptArrived {
				return unknown("the highlight did not move down toward the free-text row")
			}
			cur = now
		}
		skip = freeIdx
		rcpt, typed, refused, err := d.typeIntoFreeTextRow(ctx, target, cur, freeIdx, text, func(now *fleet.SessionPrompt) bool {
			return same(cur, now)
		})
		if err != nil || refused {
			return rcpt, err
		}
		// Typing ticks the row (measured), and an answer whose box is clear is
		// not one the runtime hands over — so the tick is read, not assumed.
		if _, ticked, _ := checkboxLabel(typed.Options[freeIdx-1]); !ticked {
			return unknown("the text reached the free-text row but its box did not read back as ticked, " +
				"so it would not be handed over")
		}
		cur = typed
		// Back onto a checkbox: Right is swallowed by the field.
		if rcpt, ok, err := moveOntoBox(); !ok {
			return rcpt, err
		}
	}

	if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, "Right"); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}
	prev := cur
	verdict, next := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
		switch {
		case now == nil:
			return promptPending // a repaint mid-frame, or the dialog closed: read on
		case same(prev, now):
			return promptPending
		}
		return promptArrived
	})

	var ticked []string
	for i := 1; i <= boxes; i++ {
		if want[i-1] {
			ticked = append(ticked, label(i))
		}
	}
	answered := "ticked no box"
	if len(ticked) > 0 {
		answered = "ticked " + strings.Join(ticked, ", ")
	}
	if len(done) > 0 {
		answered += " (" + progress() + ")"
	} else if len(ticked) > 0 {
		answered += " (already ticked exactly so; no box was flipped)"
	}
	if resp.Text != nil {
		answered += ", and " + freeTextTyped(text, freeIdx)
	}
	if resp.Nonce == "" {
		answered += "; answered without a nonce, so nothing verified the prompt " +
			"had not changed since it was read"
	}
	switch {
	case verdict == promptArrived && reviewScreenPrompt(next):
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason: answered + ", and moved on to the dialog's review screen. The " +
				"answers are NOT handed over yet: answer that screen with choice 1 " +
				"(Submit answers) and its own nonce",
		}, nil
	case verdict == promptArrived:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason:  answered + ", and moved on to the next question of the dialog",
		}, nil
	case next == nil:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason:  answered + ", and the question left the screen with no prompt in its place",
		}, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: answered + ", but the dialog did not move on: the question is still " +
			"on screen with those ticks. Read the state again before answering further",
	}, nil
}

// promptCleared waits briefly for the answered prompt to leave the screen.
//
// "Still the same prompt" is the only outcome that means the keypress did not
// register; a different prompt counts as cleared, because the session moved on
// and the caller's answer had its effect.
func (d *Driver) promptCleared(ctx context.Context, paneID, was string) bool {
	deadline := d.now().Add(promptClearWindow)
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			if now := parsePrompt(sc); now == nil || now.Nonce != was {
				return true
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(promptClearInterval):
		}
	}
}

// feedbackCardRefusalFor reads a pane afresh and names the runtime's
// feedback-draft card as the reason a delivery cannot proceed, when that is what
// the pane shows (muster#215). Fail-open on an unreadable pane: the caller
// keeps whatever wording it already had.
func (d *Driver) feedbackCardRefusalFor(ctx context.Context, paneID string) (string, bool) {
	sc, ok := d.captureForClassify(ctx, paneID)
	if !ok {
		return "", false
	}
	return feedbackNoticeRefusal(sc)
}

// receptive reports whether a pane's runtime is in a state where a keystroke
// will be received, by looking for the COMPOSER.
//
// # Why the composer is the signal, and not a timer
//
// The composer is the runtime's input widget. It is painted by the same
// component that reads keys, and it cannot appear on screen before that
// component is running — so its presence is evidence about the input path
// itself rather than about elapsed time. A clock proves nothing: the same
// wall-clock delay is comfortable on an idle machine and far too short on a
// loaded one, after a cold boot, or when the startup files a created session
// now reads are slow. That is the same class of assumption the login-shell
// work already had to stop making.
//
// # What it does NOT prove, said plainly
//
// A painted composer is strong evidence, not proof, that a keystroke will be
// consumed: the widget could in principle be drawn a moment before the input
// loop attaches. This is why it is only half the mechanism. The other half is
// confirmSubmitted, which checks AFTERWARDS whether the submit actually took —
// positive evidence rather than inference. The gate removes the common case;
// the confirmation makes the remaining case honest instead of silent.
//
// blocked reports a selection menu, which is receptive to keys but not to
// TEXT: pasting into one drives the menu instead of delivering a message.
func (d *Driver) receptive(ctx context.Context, paneID string) (ready, blocked bool) {
	sc, ok := d.captureForClassify(ctx, paneID)
	if !ok {
		// Fail closed: an unreadable pane is not a receptive one. Returning
		// "ready" here on the theory that the capture is probably fine is how
		// this defect is reintroduced.
		return false, false
	}
	if _, b := selectionPrompt(sc); b {
		return false, true
	}
	// composerClipped counts as receptive too (muster#134): a painted
	// composer this driver could not read in full is still a painted
	// composer — the runtime is up and taking input, which is all this
	// gate decides. What it does NOT decide is whether that composer is
	// safe to write into; that is Send's own composerText check, further
	// down the call path, which fails closed for exactly this scan result.
	_, scan := composerText(sc)
	return scan != composerAbsent, false
}

// awaitReceptive waits, within the call's own budget, for the runtime to be
// able to receive input.
//
// It does NOT wait for startup. §4.4 bounds every call by the driver's
// declared deadline, and a runtime takes far longer to paint than that
// deadline allows — so a Send that blocked until a freshly created session was
// ready would be a driver overrunning its own declaration. What this covers is
// the short race, and beyond that it reports not-ready so the caller is told
// rather than silently stranded.
//
// settleNewSession keeps its own, much longer readiness loop for the one
// case where waiting out a full startup is the point.
func (d *Driver) awaitReceptive(ctx context.Context, paneID string) (ready, blocked bool) {
	deadline := d.now().Add(sendReceptiveWindow)
	for {
		ready, blocked = d.receptive(ctx, paneID)
		if ready || blocked {
			return ready, blocked
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return false, false
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(sendReceptiveInterval):
		}
	}
}

func (d *Driver) confirmSubmitted(ctx context.Context, paneID string, key pasteKey, atCount int) bool {
	start := d.now()
	deadline := start.Add(submitConfirmWindow)
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			// composerClipped must NOT confirm here (muster#134):
			// unlike composerFound with text=="", a clipped screen carries
			// no assurance the composer is actually empty — it is simply
			// unreadable in full. Only an explicit composerFound-and-empty
			// reading counts as confirmation; everything else keeps
			// polling exactly as it already did for "no composer this
			// capture" (scan == composerAbsent).
			if text, scan := composerText(sc); scan == composerFound && text == "" {
				d.recordConfirmed(counterSubmitConfirmedByComposerEmpty, d.now().Sub(start))
				return true
			}
			// #180 L2: the marker is counted inside the composer only, from
			// the same capture, in the shape the landed check counted it.
			if atCount > 0 && composerMarkers(sc)[key] < atCount {
				d.recordConfirmed(counterSubmitConfirmedByMarkerCleared, d.now().Sub(start))
				return true
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			d.counters.incr(counterSubmitConfirmTimeout)
			return false
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterSubmitConfirmTimeout)
			return false
		case <-time.After(submitConfirmInterval):
		}
	}
}

// recordConfirmed increments the counter naming which signal decided a
// confirmSubmitted call, plus the latency bucket it took to decide it.
// Factored out only because confirmSubmitted now has two call-site returns
// that both need it; it carries no logic of its own beyond confirmLatencyBucket.
func (d *Driver) recordConfirmed(bySignal string, elapsed time.Duration) {
	d.counters.incr(bySignal)
	d.counters.incr(confirmLatencyBucket(elapsed))
}

// captureForClassify reads a pane in THE shape the classifier requires.
//
// # Why this exists as a function rather than as a convention
//
// Every screen the classifier parses must carry escape sequences. `newScreen`
// keeps a `raw` copy of each line for one reason, stated on the type: the
// composer's placeholder is distinguished from text a human typed by DIMNESS
// ALONE. There is no wording difference to fall back on — the hint is ordinary
// prose — so a capture without `-e` cannot express the difference at all, and
// `composerText` reports the placeholder as unsent input.
//
// That is not a cosmetic error. A composer believed to hold unsent input makes
// `Send` refuse, so an idle session showing a hint becomes UNREACHABLE through
// the API, and the refusal blames an operator who does not exist. Three
// separate sites shipped with the flag missing, each added in good faith,
// because the flag is easy to omit and nothing objected.
//
// So the shape is owned here instead of being repeated. Callers ask for "a
// screen for classification" and cannot express a wrong one.
//
// # The shape, and why these flags
//
//	-p  write to stdout rather than a buffer.
//	-e  KEEP escape sequences. The whole point; see above.
//	-S  start N lines back, so the classifier sees the tail it reasons about.
//
// `-J` is deliberately ABSENT. It joins wrapped lines, and the classifier
// already handles continuation lines itself — a long message wrapping below
// the prompt is expected and parsed. More to the point, this is the shape the
// batched enumeration has always used, which is the path every status in this
// fleet has been read through; adopting a different one here would change what
// the classifier sees everywhere, on no evidence.
// classifyCaptureArgs is THE argv shape, and the only place it is written.
//
// Separate from captureForClassify because the batched enumeration cannot call
// that helper: it packs many captures into ONE subprocess invocation, which is
// the constant-spawn property its own doc comment calls load-bearing. Sharing
// the argv rather than the function is what keeps the two paths from drifting,
// which is the drift this whole issue is about.
func classifyCaptureArgs(paneID string, lines int) []string {
	return []string{"capture-pane", "-p", "-e", "-t", paneID, "-S", "-" + strconv.Itoa(lines)}
}

func (d *Driver) captureForClassify(ctx context.Context, paneID string) (screen, bool) {
	// The pane height rides in the same invocation, AFTER the capture, as a
	// marker-prefixed last line (muster#169). After, so the capture
	// argv still begins with capture-pane; marker-prefixed, so a pane whose
	// own last row happens to be a bare number is never read as a height.
	mark := d.nonce() + "H"
	args := append(classifyCaptureArgs(paneID, d.captureLines),
		";", "display-message", "-t", paneID, "-p", mark+paneHeightFormat)
	out, err := d.run(ctx, d.bin, args...)
	if err != nil {
		// Fail closed, and let the caller decide what that means. An
		// unreadable pane is not an empty one.
		return screen{}, false
	}
	return splitHeightTrailer(string(out), mark).screen(), true
}

// splitHeightTrailer separates captureForClassify's output into the capture
// and the pane height printed after it. Only the output's LAST line is ever
// considered: the capture comes first, so a row inside it that looks like the
// trailer is pane content. No trailer, or one that does not parse, leaves the
// whole output as the capture with height 0 (unknown).
func splitHeightTrailer(out, mark string) paneCapture {
	body := strings.TrimSuffix(out, "\n")
	nl := strings.LastIndex(body, "\n")
	last := body[nl+1:]
	if !strings.HasPrefix(last, mark) {
		return paneCapture{text: out}
	}
	c := paneCapture{text: body[:nl+1]}
	if h, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(last, mark))); err == nil && h > 0 {
		c.height = h
	}
	return c
}
