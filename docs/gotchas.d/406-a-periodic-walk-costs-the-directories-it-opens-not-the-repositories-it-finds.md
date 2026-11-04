# A periodic walk costs the directories it opens, not the repositories it finds

**Issue:** #406

## What happened

On a busy host (load average about sixteen, about a thousand processes) a
five-second stack sample of the service showed between 15 % and 30 % of one
core in one `lstat` on one thread (two samples, two windows), beside directory
opens and reads on the same thread. The
binary was stripped, so the thread could not be tied to a function, and the
sample alone could not have named one anyway: the Go frames end at the runtime's
switch to the system stack, so a native sampler sees the system call and nothing
above it.

What named the owner was a CPU profile of the real periodic trust-seed pass
(`discoverIslands`) over the real configured roots, plus the open directory
handles of the live process, which sat deep inside repositories. One pass:

| | before | after |
|---|---|---|
| directories listed | 104 954 | 19 364 |
| `lstat` calls | 104 954 | 0 |
| CPU per pass | about 7.8 s | about 1.9 s |
| islands found | 182 | 182 |

Run back to back for a five-second sample, the old pass put `lstat` at 45 % of
one core; the new pass has no `lstat` in the sample at all. Of the old pass's
CPU, `os.Lstat` was 26 % and the listing of each directory (open,
read, close) was 72 %. Two things made it that large:

- **One `Lstat` per directory, on top of the listing.** `hasGitEntry` asked the
  filesystem a second question of every directory the walk was already about to
  list, and the listing already contains the answer: a `.git` entry is a name in
  it.
- **The walk descended without limit into every repository.** The configured
  roots here are themselves a repository holding repositories holding worktrees,
  so "stop at the first `.git`" is not available, and nothing else bounded it.
  63 % of the directories in a pass were one application's generated file cache,
  which names its entries by hash prefix: two hex levels of up to 256
  directories each, 65 536 directories no repository will ever live in.

## The rule

A periodic walk's cost is the number of directories it opens times what it asks
of each, so price both:

- Read a directory once. Whether it is a repository is in the listing taken to
  descend into it (`hasGitName`); a directory whose listing fails is the one
  case that still asks directly, because a directory that can be searched but not
  listed can still hold a `.git` entry.
- Bound the depth below the nearest enclosing repository
  (`maxDepthBelowIsland`), restarting the count in every repository found. The
  deepest nesting measured on the real roots was four; the bound is five. What
  the bounded pass leaves out, `SeedPath` still seeds, unbounded, just before a
  session starts in that directory, which is why the bound can be there.

Pinned by `TestSeedAllPassListsEachDirectoryOnceAndAsksAboutNone`,
`TestSeedAllPassDescendsAtMostMaxDepthBelowAnIsland`,
`TestSeedPathStillSeedsARepositoryTheBoundedPassLeavesOut` and
`TestSeedAllPassAsksDirectlyAboutADirectoryItCannotList`.

## What this did not change

The interval (two minutes), the symbolic-link behaviour (not followed, as
before), the skipped names, and what an island is. If the service's average CPU
is still above target, the next lever is the interval, not the walk.
