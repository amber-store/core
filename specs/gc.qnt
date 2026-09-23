// GC vs. two-phase ingest (object writes, then reference publication),
// ported with the mark-and-sweep collector from Mic92's bitmap GC
// (draganm/amber-store#9). Sweep policies for unmarked objects:
//   none     delete all
//   epoch    keep if written after mark started
//   horizon  keep if written after the oldest in-flight ingest started
//   quiesce  mark waits for ingests to drain. No ingest or reference
//            publication may overlap mark..sweep (stop-the-world for
//            writers only)
//   barrier  mark waits for ingests to drain, then writers run freely;
//            every key a batch observes (written or dedup-hit) during
//            the cycle joins a grey set that sweep also keeps
// Result: none/epoch lose the commit-then-publish race, horizon loses the
// dedup race (a dedup hit keeps the condemned object's old epoch).
// Quiesce and barrier are safe. Readers and liveness (writer starvation)
// are out of scope. "roots" abstracts the reference records: "publish" is
// a reference PUT, "unpublish" a reference delete. The model's publish
// does not re-check object presence; amber-store-core's PrepareRef is
// stronger — it re-walks the root's closure (failing cleanly on a missing
// object instead of dangling) and hands the walked closure to the grey
// set, which is how a PUT landing between mark and sweep stays safe
// without draining ingests before the mark.
//
// Across processes (architecture/packstore.md, "The gate") the collector
// holds the writers and the reference publication of other processes to
// the quiesce policy checked here, through one file lock, and keeps barrier
// for the writers of its own process, whose grey set it can see.
//
// All maps are total over fixed domains (absent = epoch -1 / active flag)
// so Apalache encodes them as constant-shape arrays.
module gc {
  const PROTECT: str

  // shared dependency between the two paths: breaks the horizon policy
  val refs = Map(
    "a1" -> Set("a2", "shared"),
    "a2" -> Set(),
    "b1" -> Set("shared"),
    "shared" -> Set()
  )
  val objs = refs.keys()
  val pathRoot = Map("A" -> "a1", "B" -> "b1")
  val paths = pathRoot.keys()
  // literal transitive closures, checked against refs by closureDefTest
  val closureOf = Map(
    "a1" -> Set("a1", "a2", "shared"),
    "a2" -> Set("a2"),
    "b1" -> Set("b1", "shared"),
    "shared" -> Set("shared")
  )
  def closure(r: str): Set[str] = closureOf.get(r)
  def min2(a: int, b: int): int = if (a < b) a else b

  var clock: int
  var store: str -> int              // object -> write epoch, -1 = absent
  var roots: Set[str]                // published index roots / pins
  var ingActive: str -> bool
  var ingStart: str -> int
  var ingCommitted: str -> bool
  var gcPhase: str                   // "idle" | "marked"
  var gcMarked: Set[str]
  var grey: Set[str]
  var gcStart: int
  var gcHorizon: int

  def stored(o: str): bool = store.get(o) >= 0
  val activePaths = paths.filter(p => ingActive.get(p))
  val drains = PROTECT == "quiesce" or PROTECT == "barrier"
  def greyAdd(ks: Set[str]): Set[str] =
    if (PROTECT == "barrier" and gcPhase == "marked") grey.union(ks) else grey

  action gcUnchanged = all {
    gcPhase' = gcPhase, gcMarked' = gcMarked,
    gcStart' = gcStart, gcHorizon' = gcHorizon,
  }

  action inflightUnchanged = all {
    ingActive' = ingActive, ingStart' = ingStart, ingCommitted' = ingCommitted,
  }

  action ingestUnchanged = all {
    store' = store, roots' = roots, inflightUnchanged,
  }

  action init = all {
    clock' = 0,
    store' = objs.mapBy(o => -1),
    roots' = Set(),
    ingActive' = paths.mapBy(p => false),
    ingStart' = paths.mapBy(p => 0),
    ingCommitted' = paths.mapBy(p => false),
    gcPhase' = "idle",
    gcMarked' = Set(),
    grey' = Set(),
    gcStart' = 0,
    gcHorizon' = 0,
  }

  // dedup: an already-present object keeps its epoch
  def writeObjects(objsToWrite: Set[str]): str -> int =
    objs.mapBy(o => if (objsToWrite.contains(o) and not(stored(o))) clock else store.get(o))

  action beginIngestP(p: str): bool = all {
    not(ingActive.get(p)),
    PROTECT != "quiesce" or gcPhase == "idle",
    ingActive' = ingActive.put(p, true),
    ingStart' = ingStart.put(p, clock),
    ingCommitted' = ingCommitted.put(p, false),
    clock' = clock + 1,
    store' = store, roots' = roots,
    grey' = grey,
    gcUnchanged,
  }

  // WriteBatch. Dedup keeps the existing record's epoch, but under
  // barrier every observed key goes grey.
  action commitBatchP(p: str): bool = all {
    ingActive.get(p),
    not(ingCommitted.get(p)),
    store' = writeObjects(closure(pathRoot.get(p))),
    grey' = greyAdd(closure(pathRoot.get(p))),
    ingCommitted' = ingCommitted.put(p, true),
    ingActive' = ingActive, ingStart' = ingStart,
    clock' = clock + 1,
    roots' = roots,
    gcUnchanged,
  }

  // torn WriteBatch: arbitrary durable prefixes arise from single-object
  // writes interleaved with abortIngest
  action partialWrite = {
    nondet p = oneOf(paths)
    nondet o = oneOf(objs)
    all {
    ingActive.get(p),
    not(ingCommitted.get(p)),
    closure(pathRoot.get(p)).contains(o),
    store' = writeObjects(Set(o)),
    grey' = greyAdd(Set(o)),
    inflightUnchanged,
    clock' = clock + 1,
    roots' = roots,
    gcUnchanged,
    }
  }

  // ingest dies between phases, leaving committed objects as orphans
  action abortIngestP(p: str): bool = all {
    ingActive.get(p),
    ingActive' = ingActive.put(p, false),
    ingStart' = ingStart, ingCommitted' = ingCommitted,
    clock' = clock + 1,
    store' = store, roots' = roots,
    grey' = grey,
    gcUnchanged,
  }

  // index root swap. It deliberately does not re-check object presence.
  action publishP(p: str): bool = all {
    ingActive.get(p),
    ingCommitted.get(p),
    roots' = roots.union(Set(pathRoot.get(p))),
    ingActive' = ingActive.put(p, false),
    ingStart' = ingStart, ingCommitted' = ingCommitted,
    clock' = clock + 1,
    store' = store,
    grey' = grey,
    gcUnchanged,
  }

  // eviction: an index root swap too, so quiesce blocks it during a
  // cycle. Barrier tolerates it (the stale mark keeps the closure one
  // cycle).
  action unpublishP(p: str): bool = all {
    roots.contains(pathRoot.get(p)),
    PROTECT != "quiesce" or gcPhase == "idle",
    roots' = roots.exclude(Set(pathRoot.get(p))),
    clock' = clock + 1,
    store' = store, inflightUnchanged,
    grey' = grey,
    gcUnchanged,
  }

  action beginIngest = { nondet p = oneOf(paths) beginIngestP(p) }
  action commitBatch = { nondet p = oneOf(paths) commitBatchP(p) }
  action publish = { nondet p = oneOf(paths) publishP(p) }
  action abortIngest = { nondet p = oneOf(paths) abortIngestP(p) }
  action unpublish = { nondet p = oneOf(paths) unpublishP(p) }

  action gcMark = all {
    gcPhase == "idle",
    not(drains) or activePaths.size() == 0,
    gcMarked' = roots.map(r => closure(r)).flatten(),
    grey' = Set(),
    gcStart' = clock,
    gcHorizon' = activePaths.fold(clock, (m, p) => min2(m, ingStart.get(p))),
    gcPhase' = "marked",
    clock' = clock + 1,
    ingestUnchanged,
  }

  def keepUnmarked(epoch: int): bool =
    if (PROTECT == "epoch") epoch >= gcStart
    else if (PROTECT == "horizon") epoch >= gcHorizon
    else false

  val kept = if (PROTECT == "barrier") gcMarked.union(grey) else gcMarked

  action gcSweep = all {
    gcPhase == "marked",
    store' = objs.mapBy(o =>
    if (kept.contains(o) or keepUnmarked(store.get(o))) store.get(o) else -1),
    gcPhase' = "idle",
    gcMarked' = gcMarked, grey' = grey,
    gcStart' = gcStart, gcHorizon' = gcHorizon,
    clock' = clock + 1,
    roots' = roots, inflightUnchanged,
  }

  action step = any {
    beginIngest, commitBatch, partialWrite, publish, abortIngest, unpublish,
    gcMark, gcSweep,
  }

  val safe = roots.forall(r => closure(r).forall(o => stored(o)))
  val storeEmpty = objs.forall(o => not(stored(o)))

  // inductive, for quiesce and barrier: while marked, every published
  // root's closure is inside what sweep keeps, and (barrier) so is every
  // committed in-flight ingest's. The type bounds make this a full state
  // predicate, as induction requires.
  val safeInd = and {
    clock.in(Nat),
    store.in(objs.setOfMaps(Int)),
    objs.forall(o => store.get(o) >= -1),
    roots.in(objs.powerset()),
    ingActive.in(paths.setOfMaps(Set(false, true))),
    ingStart.in(paths.setOfMaps(Int)),
    ingCommitted.in(paths.setOfMaps(Set(false, true))),
    gcPhase.in(Set("idle", "marked")),
    gcMarked.in(objs.powerset()),
    grey.in(objs.powerset()),
    gcStart.in(Int),
    gcHorizon.in(Int),
    safe,
    paths.forall(p => (ingActive.get(p) and ingCommitted.get(p)) implies
    closure(pathRoot.get(p)).forall(o => stored(o))),
    gcPhase == "marked" implies and {
    PROTECT != "quiesce" or activePaths.size() == 0,
    roots.map(r => closure(r)).flatten().subseteq(kept),
    PROTECT != "barrier" or paths.forall(p =>
      (ingActive.get(p) and ingCommitted.get(p)) implies
      closure(pathRoot.get(p)).subseteq(kept)),
    },
  }

  run closureDefTest = {
    def expand(s: Set[str]): Set[str] = s.union(s.fold(Set(), (acc, o) => acc.union(refs.get(o))))
    init.expect(objs.forall(r => closureOf.get(r) == expand(expand(expand(Set(r))))))
  }

  run abortOrphanTest =
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(abortIngestP("A"))
        .then(gcMark).then(gcSweep)
        .expect(safe and storeEmpty)

  run collectsTest =
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A"))
        .then(unpublishP("A")).then(gcMark).then(gcSweep)
        .expect(safe and storeEmpty)

  // mark runs between WriteBatch and the root swap
  run commitPublishRaceTest =
    if (drains)
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(gcMark).fail()
    else
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(gcMark)
          .then(publishP("A")).then(gcSweep)
          .expect(if (PROTECT == "horizon") safe else not(safe))

  // B's WriteBatch dedups against the condemned "shared" object
  run dedupRaceTest =
    if (drains)
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A")).then(unpublishP("A"))
          .then(beginIngestP("B")).then(commitBatchP("B")).then(gcMark).fail()
    else
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A")).then(unpublishP("A"))
          .then(beginIngestP("B")).then(commitBatchP("B"))
          .then(gcMark).then(gcSweep).then(publishP("B"))
          .expect(not(safe))

  // the dedup race moved inside the cycle: B's batch dedups against the
  // condemned "shared" after mark. Only the grey set saves it.
  run cycleDedupRaceTest =
    if (PROTECT == "quiesce")
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A")).then(unpublishP("A"))
          .then(gcMark).then(beginIngestP("B")).fail()
    else
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A")).then(unpublishP("A"))
          .then(gcMark).then(beginIngestP("B")).then(commitBatchP("B"))
          .then(gcSweep).then(publishP("B"))
          .expect(if (PROTECT == "barrier") safe else not(safe))

  // eviction during a cycle: the stale mark keeps the closure one extra
  // cycle, the next one collects it
  run cycleEvictionTest =
    if (PROTECT == "quiesce")
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A"))
          .then(gcMark).then(unpublishP("A")).fail()
    else
    init.then(beginIngestP("A")).then(commitBatchP("A")).then(publishP("A"))
          .then(gcMark).then(unpublishP("A")).then(gcSweep)
          .then(gcMark).then(gcSweep)
          .expect(safe and storeEmpty)
}

module gcNone    { import gc(PROTECT = "none").* }
module gcEpoch   { import gc(PROTECT = "epoch").* }
module gcHorizon { import gc(PROTECT = "horizon").* }
module gcQuiesce { import gc(PROTECT = "quiesce").* }
module gcBarrier { import gc(PROTECT = "barrier").* }
