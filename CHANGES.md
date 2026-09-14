# Changes against the reviewed repository

This package is a corrected/reworked version aligned to the supplied production prompt.

Major corrections:

1. **Safe aggregation** — only two real sibling prefixes may collapse into their parent. The address-space invariant is checked after eliminating duplicate/covered prefixes.
2. **Per-service isolation** — all RouterOS reads/writes are scoped to an exact `AUTO:<service>` comment; diff application cannot touch another service.
3. **Fail-closed sync** — zero collected/validated prefixes is an error and leaves RouterOS untouched.
4. **Diff + rollback attempt** — only changed routes are removed/added. An add failure triggers restoration of the service's pre-sync route set.
5. **Source classification and fallbacks** — domain/IP/ASN recognition, known CDN providers, BGPView + RIPEstat fallback, official Cloudflare/AWS/Google/Fastly sources, and `static_url` overrides.
6. **Prefix guards** — private, loopback, link-local, multicast, unspecified and overly broad ranges are filtered.
7. **Scheduling** — service → group → global resolution, cron and human-readable schedule parsing, and per-service overlap prevention in the core.
8. **Control planes** — CLI, authorized Telegram bot, embedded Web UI, REST API, CSRF protection, Basic Auth and LAN CIDR allow-list.
9. **Operations** — bbolt cache, JSON `slog`, log rotation, dry-run, Docker/LXC-oriented static build, config mode 0600 on application writes.
10. **Toolchain** — Docker builders and `go.mod` target Go 1.27.1 as requested.

## Verification performed in this environment

- `gofmt` completed successfully for all Go sources.
- Focused unit tests for the dependency-free aggregator and validator packages pass under the locally available Go toolchain.
- A full `go test ./...` could not be executed here because the local environment has Go 1.23.2 and no outbound module download access, while this project intentionally targets Go 1.27.1 and external modules.

## Second review pass

Additional defects found and corrected during the follow-up review:

- Fixed a real syntax error in `cmd/app/main.go` (an extra closing brace in `yamlGet`) that prevented compilation.
- Normalization now accepts pasted URLs and extracts the hostname instead of trying to resolve `host/path` as a DNS name.
- Existing config files are forced to mode `0600` on load, not only when the application writes them.
- RouterOS diff logic now handles duplicate service routes deterministically, rejects malformed desired routes, and attempts rollback after partial remove failures as well as add failures.
- Scheduler implementation was rewritten to honor `scheduler.parallel` and `max_concurrent`, reject unsupported schedule values through a reusable validator, and support safe runtime rebuilds after schedule changes.
- Fixed a scheduler-generation semaphore race during reload by capturing the generation's semaphore per dispatched job.
- Web schedule updates immediately reload the scheduler.
- Web API now rejects unknown services and malformed JSON, caps request bodies, and gives background syncs a finite deadline.
- WebSocket origin checking is now same-origin instead of unconditional acceptance.
- Basic Auth credentials use constant-time comparison.
- HTTP server now has read/write/idle timeouts and refuses to start when the web interface is disabled.

Verification on this pass:

- `gofmt` succeeds for every Go source file.
- Dependency-free unit tests for `internal/aggregator` and `internal/validator` pass under the available Go 1.23.2 toolchain after changing only the temporary test copy's `go` directive.
- All embedded HTML templates parse successfully with `html/template`.
- Full dependency-aware `go test ./...` is still blocked by this environment's lack of outbound Go module access; the production tree remains on Go 1.27.1, which is the current release required by the supplied prompt.
