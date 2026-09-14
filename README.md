# mikrotik-route-sync

Production-oriented Go service for automatic, **per-service** synchronization of RouterOS v7 routes.

## What it does

A user supplies a service name, domain, IP address, or ASN. The application classifies the target, collects prefixes from official CDN lists or ASN data, validates and aggregates them, and synchronizes only RouterOS routes carrying the exact comment `AUTO:<service>`.

Key properties:

- automatic classification: `asn`, `cdn`, `dynamic`, `whois`, `static_url`;
- sources: BGPView with RIPEstat fallback, Cloudflare, AWS CloudFront, Google, Fastly and Akamai/ASN fallback;
- DNS-based dynamic collection for services such as Google/YouTube;
- bbolt cache for ASN/prefix lookups;
- safe validation (private/link-local/multicast/wide prefixes are rejected);
- CIDR aggregation that merges only true sibling prefixes and verifies address-count invariance;
- exact per-service RouterOS isolation using `AUTO:<service>`;
- diff-based route updates with rollback attempt when an add fails;
- retry + circuit breaker for RouterOS REST;
- CLI, scheduler, Telegram bot, REST API and embedded Web UI;
- JSON logging with rotation;
- static `CGO_ENABLED=0` build and minimal scratch image.

## Requirements

- Go **1.27.1**
- MikroTik RouterOS v7 REST API enabled
- a dedicated RouterOS user with the minimum permissions required to read/write routes and use REST
- network access to the configured external prefix sources

## Build

```bash
cp config.example.yaml config.yaml
chmod 600 config.yaml
go mod tidy
go test ./...
CGO_ENABLED=0 go build -trimpath -o app ./cmd/app
```

## RouterOS preparation

Enable HTTPS/REST according to your RouterOS policy, create a dedicated API user, and restrict its source addresses. Prefer a valid certificate and `verify_ssl: true` in production.

The application only manages routes whose comment is exactly:

```text
AUTO:<normalized-service-name>
```

It never intentionally deletes routes belonging to another service.

## CLI

```text
app sync
app sync --service instagram
app sync --group social
app sync --dry-run
app add-service youtube
app remove-service rutor
app info cloudflare
app list
app schedule list
app schedule reload
app bot
app web
app config get mikrotik.host
app config set mikrotik.gateway wg-vpn
app config edit
app config reload
app logs tail -n 100
app logs tail -f
app logs clear
app logs size
app test-telegram
app test-mikrotik
```

## Scheduling

Resolution order is service → group → global. Supported forms:

```text
0 */6 * * *
every 6h
every 30m
daily at 03:00
weekly on sunday at 04:00
manual
disabled
inherit
```

The service-level lock prevents overlapping syncs for the same service. A different service can still be processed independently.

## Web UI and API

Run:

```bash
./app --config config.yaml web
```

Pages:

- `/` dashboard
- `/services`
- `/schedules`
- `/settings`
- `/logs`

API:

- `GET /api/v1/status`
- `GET /api/v1/services`
- `POST /api/v1/services/sync`
- `POST /api/v1/services/{name}/sync`
- `GET /api/v1/schedules`
- `PUT /api/v1/schedules/{name}`
- `GET /api/v1/logs`
- WebSocket `/api/v1/ws`

Mutating HTTP requests require the embedded CSRF token. Basic Auth can be enabled and `web.allowed_cidrs` can restrict access to trusted LAN/VPN ranges.

## Telegram bot

Enable `telegram.enabled`, set `bot_token`, `chat_id`, and `authorized_chat_ids`, then run:

```bash
./app --config config.yaml bot
```

Commands: `/start`, `/menu`, `/status`, `/sync`, `/schedule`, `/help`. Inline selection and confirmation are supported. Unauthorized chat IDs are ignored.

## Adding a service

```bash
./app add-service example.com
```

The command first resolves/classifies and synchronizes only that service. It is written to `config.yaml` only after a successful sync, avoiding a configured service with no usable prefix source.

For special cases use `overrides`:

```yaml
overrides:
  rutor:
    domains: [rutor.info, rutor.org]
    max_asn_prefixes: 50
  custom-list:
    method: static_url
    static_url: https://example.org/prefixes.txt
```

## Failure safety

If collection or validation yields zero prefixes, the existing RouterOS routes are left untouched. Updates are calculated from the exact service comment, deletions/additions are limited to that service, and an add failure triggers a best-effort restoration of the pre-sync route set.

Always keep an independent RouterOS backup before deploying route automation.

## Docker / LXC

```bash
docker compose up -d --build
```

The default image is a static scratch image. `Dockerfile.alpine` is supplied when timezone data or an interactive runtime shell is useful.

For Proxmox LXC, build the binary on amd64 and run it under a supervisor (systemd/OpenRC). Mount a persistent directory for `cache.db`, keep `config.yaml` mode `0600`, and persist the log directory.

## Notes

`config.yaml` is intentionally gitignored because the requested deployment model stores secrets in that file. Environment overrides are also supported for `MRS_MIKROTIK_PASSWORD`, `MRS_TELEGRAM_BOT_TOKEN`, and `MRS_WEB_PASSWORD`.
