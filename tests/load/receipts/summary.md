# VibeShell capacity and soak receipt

- schema: `vibeshell.load.receipt/1`
- generated: 2026-10-03T19:51:39Z
- harness: `tests/load` at `45f05aa5be34`
- provider time: deterministic delayed double (live calls: false)
- go: go1.27.1, CPUs: 16, GOMAXPROCS: 16, memory: 15.5 GiB
- cgroup cpu.max: `max 100000`, memory.max: `max`
- verdict: **all 5 scenarios met their bounds on this host**

## Scenarios

### idle-sessions-and-churn (pass, 25290 ms)

100 concurrent idle sessions with keepalives held open while 10 clients run 4 connect/disconnect rounds; achieved connections, memory, and CPU are reported rather than assumed.

- metrics:
  - churn_connections_failed: 0
  - churn_connections_opened: 40
  - coordinator_sessions_active_at_end: 0
  - cpu_ms_per_idle_session: 11.39
  - database_bytes: 5345096
  - end_goroutines: 7
  - end_heap_inuse_bytes: 34512896
  - end_open_fds: 11
  - end_rss_bytes: 56557568
  - event_writer_max_depth: 101
  - event_writer_rejected: 0
  - event_writer_submitted: 720
  - handler_sessions_opened: 140
  - idle_sessions_achieved: 100
  - idle_sessions_failed: 0
  - idle_sessions_requested: 100
  - keepalives_failed: 0
  - keepalives_sent: 400
  - peak_client_connections: 101
  - peak_rss_bytes: 56557568
  - peak_rss_human: 53.9 MiB
  - rss_bytes_per_idle_session: 565575.68
  - rss_bytes_per_idle_session_human: 552.3 KiB
  - session_drain_ms: 282
  - sessions_residual_after_drain: 0

### interactive-input-mix (pass, 14866 ms)

20 concurrent SSH clients running 3 rounds of a realistic mix (commands=8 keystroke_batches=3 pager_keys=3 top_refreshes=2 large_output=1 large_paste=1 resizes=1); local event latency is measured from the client's final input byte to the prompt that follows the turn, with the model-free engine so provider time is exactly zero.

- local event p95: 429.97 ms
- provider time (injected double): p50 0.00 ms, p95 0.00 ms over 0 requests
- metrics:
  - coordinator_turn_queue: 4
  - count_command: 480
  - count_editor_keystrokes: 180
  - count_large_output: 60
  - count_large_paste: 60
  - count_pager_navigation: 180
  - count_resize: 60
  - count_top_refresh: 120
  - cpu_ms_per_input: 7.73859649122807
  - database_bytes: 23634096
  - event_writer_max_depth: 101
  - event_writer_rejected: 0
  - event_writer_submitted: 10705
  - handler_inputs_by_kind: map[command:700 key:down:60 key:pagedown:60 key:pageup:60 paste:60 resize:160]
  - inputs_over_50ms: 855
  - inputs_refused_by_app: 0
  - local_event_max_ms: 651.529064
  - local_event_p50_ms: 329.65209
  - local_event_p95_ms: 429.967353
  - local_event_p99_ms: 595.538447
  - measured_inputs: 1140
  - p95_ms_command: 562.356317
  - p95_ms_editor_keystrokes: 0.212654
  - p95_ms_large_output: 420.190482
  - p95_ms_large_paste: 400.054788
  - p95_ms_pager_navigation: 354.943748
  - p95_ms_resize: 0.032041
  - p95_ms_top_refresh: 337.89473
  - prompts_written: 980
  - provider_time_ms: 0
  - transport_dropped_bytes: 0
- note: local event p95 was 429.97 ms, above the PLAN 13 goal of 50 ms; 855 of 1140 measured inputs exceeded it

### admitted-model-requests (pass, 15687 ms)

16 sessions submit 4 turns each through internal/routing into a harness-owned admission gate of 4 concurrent requests (max 2 per account) against a deterministic provider double delayed by 750ms; no live provider call is made and no credential is used.

- local turn (routing, admission, coordination) p95: 4502.52 ms
- provider time (injected double): p50 750.49 ms, p95 750.97 ms over 64 requests
- metrics:
  - admission_queue_wait_p95_ms: 3694.125607
  - admission_rejections: 0
  - admitted_concurrency_bound: 4
  - admitted_requests: 64
  - database_bytes: 5656512
  - event_writer_max_depth: 16
  - event_writer_rejected: 0
  - event_writer_submitted: 736
  - local_turn_p95_ms: 4502.517686
  - max_account_in_flight: 2
  - max_concurrent_in_flight: 4
  - max_waiting: 16
  - provider_cancelled: 0
  - provider_deadline_exceeded: 0
  - provider_max_in_flight: 4
  - provider_requests: 64
  - provider_time_is_injected: true
  - provider_time_p95_ms: 750.965091
  - turns_completed: 64
  - turns_submitted: 64

### world-contention (pass, 138 ms)

8 writers materialise one private path and 8 writers one shared path concurrently while 8 readers read the shared path, followed by 6 conflicting saves from the same stale revision; the real SQLite world adapter resolves the outcome.

- world read (shared path during writes) p95: 43.81 ms
- metrics:
  - conflicting_saves_accepted: 1
  - conflicts_detected: 5
  - database_bytes: 2162816
  - node_revision_after: 2
  - node_revision_before: 1
  - private_race_conflicts: 7
  - private_race_winners: 1
  - private_race_writers: 8
  - read_max_ms: 43.810044
  - read_p95_ms: 43.810044
  - shared_first_time_accepted: 8
  - shared_read_errors: 4
  - shared_reads_accepted: 11
  - shared_writers: 8
  - write_errors: 0
  - write_max_ms: 51.381285
  - write_p95_ms: 48.640275
- note: 4 of 15 shared reads returned not-found because they ran before the first writer materialised the path; they are expected and are not counted as read failures of the adapter

### sandbox-and-state-soak (pass, 3018 ms)

6 rounds of 4 concurrent sandbox instances each processing 25 events, with app state evicted and restored (65536 bytes) every round; memory, file descriptors, and database growth are compared between the first and last round.

- soak round p95: 426.43 ms
- metrics:
  - engine_imports: env.jsFunctionProxy,wasi_snapshot_preview1.args_get,wasi_snapshot_preview1.args_sizes_get,wasi_snapshot_preview1.environ_get,wasi_snapshot_preview1.environ_sizes_get,wasi_snapshot_preview1.clock_time_get,wasi_snapshot_preview1.fd_close,wasi_snapshot_preview1.fd_fdstat_get,wasi_snapshot_preview1.fd_fdstat_set_flags,wasi_snapshot_preview1.fd_prestat_get,wasi_snapshot_preview1.fd_prestat_dir_name,wasi_snapshot_preview1.fd_read,wasi_snapshot_preview1.fd_readdir,wasi_snapshot_preview1.fd_seek,wasi_snapshot_preview1.fd_write,wasi_snapshot_preview1.path_create_directory,wasi_snapshot_preview1.path_filestat_get,wasi_snapshot_preview1.path_filestat_set_times,wasi_snapshot_preview1.path_open,wasi_snapshot_preview1.path_remove_directory,wasi_snapshot_preview1.path_rename,wasi_snapshot_preview1.path_unlink_file,wasi_snapshot_preview1.poll_oneoff,wasi_snapshot_preview1.proc_exit
  - final_goroutines: 3
  - final_heap_inuse_bytes: 17129472
  - first_round_open_fds: 7
  - first_round_rss_bytes: 57241600
  - last_round_open_fds: 7
  - last_round_rss_bytes: 52883456
  - open_fd_growth: 0
  - retained_state_bytes: 65536
  - round_max_ms: 426.434318
  - round_p95_ms: 426.434318
  - rounds: 6
  - rss_growth_bytes: -4358144
  - rss_growth_human: -4.2 MiB
  - rss_growth_per_round_bytes: -871628
  - sandbox_failures: 0
  - sandbox_runs: 600

## Findings

- the revision stamp was supplied by the caller because git could not read the checkout from inside the container
- admission control for concurrent model requests is harness-owned: the service configuration validates inference.global_concurrency, inference.max_account_concurrency, and inference.wait_queue_depth, but no runtime package enforces them yet, so the bound measured here is the harness gate's, not the service's
- local event p95 was 429.97 ms against the PLAN 13 goal of 50 ms with 1140 inputs measured; the transport round trip itself stayed far under the goal (p95_ms_resize 0.032041, p95_ms_editor_keystrokes 0.212654), so the cost is in durable recording rather than in the SSH path. The SQLite event store serialises appends on a single connection with synchronous=FULL, which is the first boundary to measure; this harness observes the latency but does not prove that cause.
- local-engine scenarios produced 10705 events in the research store with a maximum writer-queue depth of 101

