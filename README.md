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
| **Project**             | Something that can be deployed, e.g. `platform-core`     |
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

Pre-v0.1. **Consumer Contract v1 is a candidate, not yet frozen** — it freezes
after contract hardening has been exercised by a real `platform-core`
deployment. The schemas are defined and `deployctl validate` enforces them;
see [docs/consumer-contract-v1.md](docs/consumer-contract-v1.md) and the v0.1
scope in [docs/architecture.md](docs/architecture.md).

## Current CLI

```
deployctl validate <manifest.yaml>...   validate project/release/environment/target manifests
deployctl version                       print version
```

Manifests pass through one authoritative pipeline: header check → JSON Schema
→ strict typed decoding → semantic invariants. The schemas are the structural
contract; `validate` is the definitive validator.

Planned (not yet implemented): `release create`, `promotion propose`,
`deploy`, `rollback`, `status`. See the roadmap in
[docs/release-lifecycle.md](docs/release-lifecycle.md).

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
