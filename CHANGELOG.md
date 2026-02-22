# Changelog — mayencenouvelle-cli

All notable changes to this project are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased] — develop branch

### Added
- `undeploy` command: stops a running app via Coolify API (`POST /applications/{uuid}/stop`)
  - Preserves app config, env vars, and git settings in Coolify
  - `--delete` flag: permanently removes the app with a 5s abort window
  - Handles "not found in Coolify" gracefully (exits 0 with a warning)
- `Stop()` and `Delete()` methods on the Coolify client
- Post-deploy FQDN reminder: warns when the domain isn't configured yet in Coolify UI
  after a first deploy (fires only when `fqdn` is genuinely unset)

### Fixed
- `WaitForHealthy`: now accepts `"running:healthy"` status in addition to `"running"` —
  Coolify returns the former and the CLI was timing out on every deploy
- `EnsureApp` (update path): returns the pre-fetched `existing` app record instead of the
  PATCH response body — Coolify PATCH omits `name` and `fqdn`, causing blank fields
- FQDN mismatch warning: normalises `http://` vs `https://` before comparing — Coolify
  stores `http://` internally; the warning no longer fires on every run after first deploy
- `deploy` output: service name now shown correctly in `✓ [Coolify] service <name> ready`

### Removed
- Traefik config file generation from the deploy pipeline: the CLI was writing a
  `<app>.yaml` file to the Traefik dynamic config directory on each deploy, which
  is not scalable and diverges from how vpn-app and internal-api are configured.
  Routing is handled by the existing wildcard rule in `coolify-forward.yml`.

### Changed
- `strings` added to `cmd/deploy.go` imports (scheme normalisation for FQDN check)
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
