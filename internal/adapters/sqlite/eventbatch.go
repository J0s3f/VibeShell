package sqlite

// Group commit for the event writer.
//
// A single acknowledged append must be durable under synchronous=FULL before
// its caller is told it succeeded. Committing one event per transaction pays a
// full WAL fsync each time, which is what makes local event p95 far exceed the
// PLAN 13 goal under a loaded queue. Group commit keeps that guarantee while
// amortizing one commit across several adjacent appends:
//
//   - The writer removes the first queued job by blocking, then drains up to
//     maxBatch-1 further jobs without blocking. An idle writer therefore still
//     commits a single event immediately, adding no latency; a saturated queue
//     forms a batch and pays one fsync for all of it.
//   - Every event in the batch is written inside one transaction and the
//     transaction commits before any caller's outcome is delivered, so an
//     acknowledged append is exactly as durable as before.
//   - Per-session sequence ordering is preserved: appendRecord reads the
//     session's current maximum sequence from inside the same transaction, so
//     several events for one session in a batch receive consecutive sequences
//     in arrival order. Individual timestamps and monotonic offsets are set by
//     the caller before append and are never rewritten here.
//
// drainBatch is deliberately non-blocking after the first job so that adding
// group commit cannot delay an append that arrives while the writer is idle.

// drainBatch returns first plus any jobs already waiting on queue, up to limit
// total. It never blocks: it takes only what is queued at the moment it runs.
func drainBatch(queue <-chan appendJob, limit int, first appendJob) []appendJob {
	batch := make([]appendJob, 1, limit)
	batch[0] = first
	for len(batch) < limit {
		select {
		case job, ok := <-queue:
			if !ok {
				return batch
			}
			batch = append(batch, job)
		default:
			return batch
		}
	}
	return batch
}
