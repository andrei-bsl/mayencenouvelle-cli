# Changelog — mayencenouvelle-cli

All notable changes to this project are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased] — develop branch

### Added
- Automatic Traefik public-router reconciliation during `deploy` for apps with
  `domains.public`:
  - Writes/updates `coolify-apps-public-managed.yml` in Traefik dynamic config dir
  - Creates one explicit `Host(...)` router per external hostname (stage-aware, e.g. `dev-*`)
  - Preserves manually maintained Traefik files (no in-place rewrite)
  - `undeploy --delete` removes the managed router entries for the targeted app/stage
- Private repository support: `EnsureApp` now routes to `POST /api/v1/applications/private-deploy-key`
  when `private_key_uuid` is set (app-level or base), instead of always using `/applications/public`.
  The `/public` endpoint silently ignores `private_key_uuid`, leaving `private_key_id: None` and
  causing SSH authentication failures at clone time.
  - `httpsToSSH()` converts repository HTTPS URLs to SSH format (`git@github.com:...`) automatically
  - `private_key_uuid` can be set per-app (`repository.private_key_uuid` in manifest) or globally
    in `base.yaml` (`coolify.private_key_uuid`)
  - GitHub deploy keys must be unique per repository; use per-app keys for multiple private repos
- `undeploy` command: stops a running app via Coolify API (`POST /applications/{uuid}/stop`)
  - Preserves app config, env vars, and git settings in Coolify
  - `--delete` flag: permanently removes the app with a 5s abort window
  - Handles "not found in Coolify" gracefully (exits 0 with a warning)
- `Stop()` and `Delete()` methods on the Coolify client
- Automated domain configuration: `EnsureApp` now sets the `domains` field via PATCH
  after creation and on every update — no manual "set domain in Coolify UI" step required.
  Uses `http://` prefix so Coolify generates plain HTTP Traefik labels compatible with the
  central-Traefik→coolify-proxy:80 forwarding architecture (not `https://` which would
  add a `redirect-to-https` + `letsencrypt` router and cause a redirect loop).

### Fixed
- `WaitForHealthy`: now accepts `"running:healthy"` status in addition to `"running"` —
  Coolify returns the former and the CLI was timing out on every deploy
- `EnsureApp` (update path): returns the pre-fetched `existing` app record instead of the
  PATCH response body — Coolify PATCH omits `name` and `fqdn`, causing blank fields
- `deploy` output: service name now shown correctly in `✓ [Coolify] service <name> ready`

### Removed
- Manual FQDN warning block from `deploy.go`: domain is now set automatically, making the
  "Manual step required" prompt obsolete
- Traefik config file generation from the deploy pipeline: the CLI was writing a
  `<app>.yaml` file to the Traefik dynamic config directory on each deploy, which
  is not scalable and diverges from how vpn-app and internal-api are configured.
  Routing is handled by the existing wildcard rule in `coolify-forward.yml`.
- `strings` import from `cmd/deploy.go` (was used for FQDN scheme normalisation in the
  now-removed manual warning block)

### Changed
- `deploy` command description updated to reflect that Traefik config is no longer written

---

## [0.1.0] — 2026-02-16 (initial working version)

### Added
- `deploy <app>` command: end-to-end deploy pipeline
  - Validates manifest
  - Creates or updates Coolify service (idempotent)
  - Injects OIDC credentials into env vars (for `auth: oidc` apps)
  - Triggers Coolify deployment
  - Waits for health check to pass
- `undeploy` — not yet added (added in develop above)
- `rollback <app>` command: triggers rollback to previous Coolify deployment
- `rotate-secret <app>` command: rotates Authentik OAuth2 credentials and updates Coolify env
- `plan <app>` command: dry-run preview of what `deploy` would change
- `validate` command: validates all manifests against JSON schema
- `list apps` command: shows all defined apps with Coolify status
- `status` command: health-checks all deployed services
- `apply-manifest` command: deploys all enabled apps in dependency order
- Coolify client: `EnsureApp`, `Deploy`, `WaitForHealthy`, `ListDeployments`, `RollbackToDeployment`
- Authentik client: `EnsureOAuth2Provider`
- Manifest loader: reads `apps/*.yaml` + `base.yaml` from the manifests directory
- JSON schema validation for `mnlab/v1` manifests
- `COOLIFY_INSECURE` env var: skip TLS verification for self-signed certs in dev
