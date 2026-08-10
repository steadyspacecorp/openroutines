# Operations

## Run history: opencode's log passed through, sessions exported

**Decision.** Run output is not part of the supervisor log, and the framework keeps no private run-artifact format.
The runner uses opencode's own interfaces: diagnostic stderr is passed through with `routine=` and `run_id=` fields, while interactive progress is discarded for unattended runs.
When `OPENROUTINES_SESSION_DIR` is set, the supervisor lists and exports the attempt's sessions after it ends into flat, timestamp-prefixed files carrying the routine, run, and session identities; the model process cannot access that storage.
Exports are written through the supervisor's capture path and rejected unless complete, so a successful command cannot silently leave a truncated session artifact.

**Why.** Using opencode's log and export interfaces keeps the framework independent of opencode's internal storage layout.
Identity is appended instead of reparsing another program's log format, and one export per session preserves subagent boundaries.
## The log is structured: slog records, logfmt output

**Decision.** The framework uses one process-wide `log/slog` logger with the stdlib text handler, writing structured logfmt records to stderr.
Messages are short constants and variable data is attributes; routine identity and run identity are fields.
The handler resolves level, destination, and timezone centrally, with an environment override for the log level.
Secrets register with the process-wide scrubber when materialized, and the logger, opencode passthrough, manual echo, and knowledge-append seam all redact from that registry.

**Why.** Structured fields keep concurrent runs filterable, while one handler and one scrub registry prevent individual call sites from bypassing logging or redaction policy.
