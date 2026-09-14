# Security notes

- Use a dedicated RouterOS REST user with least privilege and source-IP restrictions.
- Prefer HTTPS with a valid certificate and `verify_ssl: true`.
- Keep `config.yaml` mode `0600`; it may contain clear-text secrets by design.
- Restrict Web UI access with `web.allowed_cidrs`, Basic Auth, firewall rules, or a VPN.
- Keep the default exact route comment prefix. Never reuse `AUTO:<service>` for manually managed routes.
- Test new service definitions with `sync --service <name> --dry-run` before enabling a recurring schedule.
- Back up RouterOS before deploying automation.
