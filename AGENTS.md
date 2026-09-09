# AGENTS.md

Guidance for humans and AI agents contributing to Deploy Toolkit.

## What this project is

Deploy Toolkit is a **release-promotion and deployment engine**. It takes an
immutable release and makes it the state of an environment — safely,
deterministically, with a human authorization gate.

It is **not**:

- a CI system (CI Toolkit decides what may reach `main`);
- an infrastructure provisioner (Pulumi owns that);
- an application framework (the deployment contract comes from the consumer).

Stay inside that boundary. If a feature request belongs to another control
plane, say so instead of building it here.

## Hard invariants

These are load-bearing design decisions. Do not weaken them without an explicit
design discussion and a contract version bump where applicable:

1. **Releases are immutable.** A release pins source revision, artifact digests,
   bundle digest, deployment-contract digest and migration semantics. Never
   introduce `latest`, floating tags, or mutable release manifests.
2. **The deployment contract comes from the promoted release.** Deploying
   revision `abc123` must use `project.yaml`, `deploy/**` and `config/**` from
   `abc123` — never from `main`'s current state. Old app + new procedure is a
   version-skew bug class; design against it.
3. **argv, not shell strings.** Lifecycle hooks are argv vectors or versioned
   scripts. Never evaluate YAML strings as shell programs.
4. **AI never decides that production changes.** Deterministic eligibility →
   human promotion (merge) → deterministic deployment. AI may explain,
   summarize, and annotate — never authorize.
5. **Promotion PRs have a strict diff allowlist.** A promotion PR may only touch
   `.deploy/releases/<new>.yaml` and `.deploy/environments/<env>.yaml`. Anything
   else fails the promotion check, because the authorization PR must not be able
   to modify the machinery executing the deployment.
6. **No environment branches.** `main` is the only permanent branch. Environments
   are files under `.deploy/environments/`, not Git refs.
7. **Strict host verification.** Never ship `StrictHostKeyChecking=no` or
   equivalent. Targets pin their SSH host key.
8. **Secret separation.** The prepare stage holds no production secrets; the
   deploy stage holds only the target connection credential. Application secrets
   live on the target, outside the release.
9. **Consumers pin full SHAs** of this repository. Workflow interfaces are
   consumer contracts; changes to them follow the versioning policy in
   `docs/consumer-contract-v1.md`.

## Scope discipline (v0.1)

Do **not** add: Kubernetes, Terraform/Pulumi execution, cloud-provider APIs,
agent daemon, multi-host scheduler, blue/green, canary, service discovery,
secrets manager, container orchestration, package registry, dashboard, or AI
deployment decisions.

Mechanisms are earned by concrete failure modes or real consumer requirements —
never added because mature-looking systems have them.

Project-specific behavior must never leak into the toolkit. Wrong:

```go
if project == "example-service" { restartLiveKit() }
```

Right: the project declares a `verify` hook in its own `project.yaml`.

## Branch model

- `main` — the sole permanent integration branch.
- `fix/*`, `docs/*`, `chore/*`, `feature/*` — short-lived; substantial features
  use `feature/<name>/phase/<n>` commits.
- Plan documents are required for substantial features, before implementation.

## Development

```
go build ./...          compile
go vet ./...            vet
go test ./...           deterministic tests (required for behavior changes)
gofmt -l .              formatting check (CI fails on output)
go build -o deployctl ./cmd/deployctl
./deployctl validate templates/*.yaml
```

CI (`.github/workflows/ci.yml`) runs exactly these checks on every push and PR.

## Contract status

The schemas in `schemas/` are the public contract — currently **candidate
Consumer Contract v1**: it remains candidate until it has been exercised end to
end by a real external consumer in a production-shaped deployment rehearsal. Go types in `internal/manifest` must stay in
lockstep with them.

All manifest ingestion goes through **one pipeline** in `manifest.Parse`:
header → JSON Schema → strict typed decoding → semantic invariants. Never
validate by schema alone or decode without schema — `deployctl validate` and
every `Load*` function share the pipeline. Invariants JSON Schema cannot
express (untagged OCI names, `irreversible ⇒ rollbackSafe: false`, path
traversal rules) live as `check()` methods in `internal/manifest`.

If you change a schema, you are changing a published API — see the versioning
policy in `docs/consumer-contract-v1.md`.

## Reference consumer

The contract must be validated by a real external consumer in a
production-shaped deployment rehearsal; `examples/static-site` (later) proves
the generic contract with a public example. No consumer's specifics — and no
consumer's name — leak into this repository.
