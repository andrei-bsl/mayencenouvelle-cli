# mayencenouvelle-cli

`mayence` is the deployment orchestration CLI for the Mayence Nouvelle homelab.
It uses declarative YAML manifests as the single source of truth and drives
Coolify, Authentik, and Traefik through their respective APIs.

## Architecture

```
workspace/manifests/
├── base.yaml           # Global defaults and infrastructure registry
├── schema/             # JSONSchema v7 (CI validation)
└── apps/
    ├── nas-app.yaml
    ├── vpn-app.yaml
    ├── internal-api.yaml
    └── ...

mayencenouvelle-cli/
├── cmd/                # Cobra commands (validate, list, status, plan, deploy, ...)
└── internal/
    ├── manifest/       # Manifest types + YAML loader + topological sort
    ├── coolify/        # Coolify REST API client
    ├── authentik/      # Authentik REST API client
    ├── traefik/        # Traefik file-provider config generator
    ├── github/         # GitHub webhook management
    └── common/         # Shared HTTP client with retry + auth
```

## Installation

```bash
# From source
make build
make install          # installs to /usr/local/bin/mayence

# From GitHub release (linux/amd64)
curl -L https://github.com/mayencenouvelle/mayencenouvelle-cli/releases/latest/download/mayence_linux_amd64.tar.gz | tar xz
sudo mv mayence /usr/local/bin/
```

## Configuration

Environment variables (required for deployment commands):

| Variable               | Description                          |
|------------------------|--------------------------------------|
| `COOLIFY_URL`          | Coolify base URL                     |
| `COOLIFY_API_TOKEN`    | Coolify API token                    |
| `AUTHENTIK_URL`        | Authentik base URL                   |
| `AUTHENTIK_API_TOKEN`  | Authentik API token                  |
| `TRAEFIK_CONFIG_DIR`   | Path to Traefik dynamic config dir   |
| `MN_RUNTIME_SSH_TARGET` | Full SSH target for runtime checks (e.g. `mn-automation@runtime.mayencenouvelle.internal`) |
| `MN_RUNTIME_SSH_HOST` | Runtime SSH hostname (e.g. `runtime.mayencenouvelle.internal` or `lab-runtime01.internal`) |
| `MN_RUNTIME_SSH_USER` | Optional SSH user to combine with `MN_RUNTIME_SSH_HOST` (recommended technical user) |
| `MN_RUNTIME_SSH_PORT` | Optional SSH port override |
| `MN_RUNTIME_SSH_KEY_FILE` | Optional SSH private key path for runtime operations |
| `MN_TRAEFIK_RUNTIME_SSH_TARGET` | Optional Traefik-specific SSH target (defaults to runtime SSH target/host+user) |
| `MN_TRAEFIK_RUNTIME_DYNAMIC_DIR` | Runtime Traefik dynamic dir (default: `/srv/docker/infra/traefik/dynamic`) |
| `MN_TRAEFIK_API_URL`   | Traefik API base URL for router verification (default: `base.yaml` `traefik.admin_endpoint`) |
| `MN_TRAEFIK_API_INSECURE` | `true` to skip TLS verification for Traefik API |
| `MN_TRAEFIK_PUBLIC_WILDCARD_ENABLED` | `true` to use wildcard public routing mode (no per-app Host() generation) |
| `MN_TRAEFIK_PUBLIC_WILDCARD_SUFFIX` | Wildcard suffix to verify in Traefik routers (default: `apps.mayencenouvelle.com`) |
| `MN_ALLOW_STATIC_PUBLIC_ROUTERS` | Temporary bypass for legacy static router file during migration (`true`/`false`) |
| `MN_DEPLOY_STRICT_DOMAIN_CHECKS` | `true` to fail deploy on DNS/TLS/HTTP public-domain probe issues |
| `MN_PUBLIC_DNS_EXPECT_CNAME` | Optional expected CNAME substring for public domains (e.g. `nouvellemayence.ddns`) |
| `GITHUB_TOKEN`         | GitHub PAT (scope: admin:repo_hook)  |

Base manifest requirement for OIDC apps:
- `workspace/manifests/base.yaml` should define `authentik.signing_key` so new Authentik OAuth2 providers publish non-empty JWKS keys.

Or use a config file (default `~/.mayence.yaml`):
```yaml
manifests: /path/to/workspace/manifests
coolify:
  endpoint: https://coolify.apps.mayencenouvelle.internal
  token: ${env:COOLIFY_API_TOKEN}
authentik:
  endpoint: https://authentik.apps.mayencenouvelle.internal
  token: ${env:AUTHENTIK_API_TOKEN}
traefik:
  config_dir: /etc/traefik/dynamic
```

## Commands

```bash
# Validate manifests (CI gate)
mayence validate                    # all apps
mayence validate --app nas-app      # single app

# List apps
mayence list apps
mayence list apps --all             # include disabled

# Check runtime status
mayence status                      # all apps
mayence status --app nas-app

# Preview changes (dry-run)
mayence plan nas-app

# Deploy
mayence deploy nas-app              # single app: 6-step deploy (validate → auth → infra → routes → trigger → health)
mayence apply-manifest              # all apps in dependency order

# Secrets
mayence rotate nas-app              # rotate Authentik OAuth2 secret + redeploy

# Recovery
mayence rollback nas-app            # roll back to previous Coolify deployment
```

For `coolify-app` manifests with `domains.public`, deploy also performs:
- Traefik runtime file sync to the runtime host (if SSH target configured),
- Traefik router presence verification via API,
- Coolify runtime self-heal restart if API status is running but the container is missing on runtime host.
- Public domain readiness probes (DNS, TLS handshake, HTTPS status); strict mode is optional.
- Single source-of-truth enforcement for public app routers (`coolify-apps-public-managed.yml`).

## Manifest Schema

Full schema reference: [`workspace/manifests/SCHEMA.md`](../../manifests/SCHEMA.md)

Schema file: [`workspace/manifests/schema/app-config.v1.schema.json`](../../manifests/schema/app-config.v1.schema.json)

### Minimal example

```yaml
apiVersion: mnlab/v1
kind: AppConfig
metadata:
  name: my-app
  phase: "7"
spec:
  enabled: true
  type: coolify-app
  capabilities:
    auth: oidc
    exposure: internal
    tls: letsencrypt
  repository:
    url: https://github.com/mayencenouvelle/my-app
    branch: main
  runtime:
    port: 3000
  domains:
    private: my-app.apps.mayencenouvelle.internal
```

## Development

```bash
make build          # build binary
make test           # run unit tests
make lint           # golangci-lint
make validate       # validate all workspace manifests
```

## Release

Tag a version to trigger the GoReleaser workflow:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`
are published to GitHub Releases.
