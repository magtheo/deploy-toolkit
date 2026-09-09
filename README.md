# Deploy Toolkit

**Deploy Toolkit is a release-promotion and deployment engine.**

It answers one question:

> *Should this exact release become the state of this environment, and did it deploy correctly?*

It does not decide whether code is good enough to merge — that is CI Toolkit's job.
It does not provision infrastructure — that is Pulumi's job.
It does not know how your application works internally — your project declares that through a **deployment contract**.

```
               SOURCE / CHANGE CONTROL
                       │
                  CI Toolkit
                       │
               Human merge to main
                       │
                       ▼
                 immutable release
                       │
                       ▼
              ┌─────────────────┐
              │ Deploy Toolkit  │
              │                 │
              │ release         │
              │ promotion       │
              │ deployment      │
              │ rollback        │
              └────────┬────────┘
                       │
                deployment target
                       ▲
                       │
                    Pulumi
                       │
                infrastructure
```

## The four-tool architecture

| System             | Fundamental question                                                         |
| ------------------ | ---------------------------------------------------------------------------- |
| **CI Toolkit**     | *Should this change be allowed toward `main`?*                               |
| **Deploy Toolkit** | *Should this release become the state of this environment, and did it work?* |
| **Pulumi**         | *What infrastructure should exist for that environment?*                     |
| **Your app**       | *What application are we actually running?*                                  |

Compactly:

```
CI Toolkit       change → release
Deploy Toolkit   release → environment
Pulumi           specification → infrastructure
```

## Core concepts

Deploy Toolkit deliberately has a small vocabulary:

| Concept                 | Meaning                                                  |
| ----------------------- | -------------------------------------------------------- |
| **Project**             | Something that can be deployed, e.g. `example-service`   |
| **Release**             | Immutable application version + artifacts                |
| **Environment**         | Desired deployment state, e.g. `production`              |
| **Target**              | A machine/environment capable of receiving a release     |
| **Deployment Contract** | Project-specific instructions for applying a release     |
| **Promotion**           | Human authorization to make an environment use a release |
| **Deployment**          | Attempt to make the target match that promoted state     |
| **Observed State**      | What the target is actually running                      |

The most important distinction is:

```
Release ≠ Deployment
```

A release can exist without ever reaching production. Promotion is a human act;
deployment is a deterministic machine act.

## Status

Pre-v0.1. **Consumer Contract v1 is a candidate, not yet frozen.** It remains
candidate until it has been exercised end to end by a real external consumer
in a production-shaped deployment rehearsal. The schemas are defined and `deployctl validate` enforces them;
see [docs/consumer-contract-v1.md](docs/consumer-contract-v1.md) and the v0.1
scope in [docs/architecture.md](docs/architecture.md).

## Current CLI

```
deployctl validate <manifest.yaml>...      validate project/release/environment/target manifests
deployctl release create [flags]           run eligibility and create an immutable release manifest
deployctl promotion propose <env> [flags]  open the human-authorization PR for a release
deployctl promotion check [flags]          verify a PR diff against the Promotion Diff Policy
deployctl version                          print version
```

Manifests pass through one authoritative pipeline: header check → JSON Schema
→ strict typed decoding → semantic invariants. The schemas are the structural
contract; `validate` is the definitive validator.

`release create` resolves the branch head, proves the candidate is reachable
from it, verifies every required check on the exact SHA, resolves artifact
digests via the SHA discovery tag, builds the deterministic bundle from the
exact Git tree, and writes `.deploy/releases/<project>-<version>.yaml` —
idempotently, refusing to overwrite a different release with the same version.
It stops at the Release boundary.

`promotion propose` re-verifies the release against current eligibility
policy, reads the environment from trusted `main`, and opens a machine-generated
authorization PR containing one atomic commit: the release file plus an
environment change limited to `spec.release`. It is idempotent, detects
conflicting proposals, and stops at the open PR — **merging is the human
authorization act** (see
[docs/promotion-diff-policy.md](docs/promotion-diff-policy.md)).

Requires `GITHUB_TOKEN` for the GitHub API; registry auth uses the standard
OCI keychain, independent of `GITHUB_TOKEN`.

### Deploy, rollback, status

The deployment side is a thin, deliberate wrapper over the engine: every
decision — locks, staging, contract verification, marker discipline, state
transitions — lives in the lifecycle engine; the CLI loads manifests,
prepares bundles from the pinned revisions, connects with the target's
declared transport, and renders the report.

```
deployctl deploy production                      # deploy the release the environment pins
deployctl rollback production --to 0.1.16        # emergency recovery; typed confirmation
deployctl status production                      # desired vs observed + all recovery facts
```

Merging the promotion PR is the authorization for `deploy`; running the
command is not. `rollback` requires typing exactly
`rollback <env> to <version>` — deliberate friction for the emergency
path. Normal rollback of a healthy deployment is an ordinary promotion
with a reverse diff, not `rollback`.

Operational exit codes (deploy, rollback, status, recovery resolve):
`0` success, `1` reported outcome failure (determined — history records
what happened), `2` usage/configuration error, `3` infrastructure
failure. The failure **report** — never the exit code alone —
distinguishes pre-execution failures (nothing ran), bookkeeping failures
after a committed state, and uncertain outcomes (an attempt/recovery
marker is unresolved). Automation keys on the reported recovery state,
not on exit 3.

Every operational command accepts the bare `--json` flag and then writes
exactly **one** `deployctl.result/v1` document to stdout on every
terminal path — success, failure, refusal, uncertainty, infrastructure,
or usage error — with no prose. Machine mode is non-interactive
(`--confirm` required; stdin is never read). The frozen machine contract
— envelope, outcome vocabulary, per-command data shapes and versioning
policy — is [docs/cli-v1.md](docs/cli-v1.md).

For SSH targets, the manifest names environment variables and the
environment holds values: `credentialFrom` and `hostKeyFrom` name
variables whose values are **paths** to the private-key file and to the
pinned host-key file (authorized_keys format). `status` fails closed:
unreadable recovery evidence is DEGRADED EVIDENCE, never a silent
HEALTHY, and a held lock dominates marker guidance.

## For consumers

A consuming repository declares itself under `.deploy/`:

```
.deploy/
├── project.yaml                  what the project is and how a release is applied
├── releases/                     generated, immutable release manifests
│   └── my-app-0.1.17.yaml
└── environments/
    └── production.yaml           human-approved desired state
```

Promotion is a one-line diff:

```diff
 spec:
-  release: .deploy/releases/my-app-0.1.16.yaml
+  release: .deploy/releases/my-app-0.1.17.yaml
```

**Merge = authorize production.** Rollback is the same diff in reverse.

Start from [`templates/`](templates/) and read
[docs/consumer-contract-v1.md](docs/consumer-contract-v1.md).

## Design invariants

- **Releases are immutable**: source revision + exact artifact digests + bundle + deployment contract + migration semantics, one canonical manifest, no `latest`.
- **The deployment contract comes from the promoted release**, never from `main`'s current state. Old app + new deploy procedure is a bug class, not a feature.
- **argv, not shell strings.** Lifecycle hooks are argv vectors or versioned scripts, never arbitrary shell programs.
- **AI never decides that production changes.** Deterministic eligibility, human promotion, deterministic deployment.
- **No environment branches.** `main` is the only branch; environments live in `.deploy/environments/`.
- **Consumers pin full SHAs.** Readable releases are published above immutable SHAs.

See [docs/trust-model.md](docs/trust-model.md) for the full trust model and
[AGENTS.md](AGENTS.md) for contributor/agent guidance.

## Governance

Branches: `main` (permanent), plus `fix/*`, `docs/*`, `chore/*`, `feature/*` (with `phase/*` prefixes for substantial work). Substantial features get a plan first. Deterministic tests are required. AI review is advisory; humans hold merge authority.

## License

[Apache-2.0](LICENSE).
