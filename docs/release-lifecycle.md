# Release Lifecycle

From source commit to verified production state.

## The pipeline

```
Developer / agent
       │
       ▼
feature branch → Pull Request
       │
CI (deterministic) + CI Toolkit AI review (advisory)
       │
HUMAN MERGE ──► main
       │
       ▼
build + test + publish  (SHA-tagged artifacts)
       │
       ▼
deployctl release propose <env> <revision>
       │  deterministic eligibility
       ▼
promotion PR
       │
HUMAN MERGE = authorize
       │
       ▼
Deploy Toolkit deploys
```

## Release proposal

`deployctl release propose production abc123` performs deterministic
eligibility checks — no judgment, just verification:

1. revision exists and belongs to permitted `main`;
2. required GitHub checks passed;
3. artifact digests resolved;
4. deployment bundle built and digested;
5. migration metadata inspected;
6. release manifest generated and validated;
7. environment file updated;
8. promotion PR opened.

```
$ deployctl release propose production abc123

✓ source revision exists
✓ revision belongs to main
✓ required CI passed
✓ app artifact found
✓ orchestrator artifact found
✓ workspace artifact found
✓ bundle constructed
✓ manifest validated

Opened:
deploy: promote platform-core 0.1.17 to production
```

## Promotion PR

The human gate. Machine-generated, boring, and read like a changelog:

```
Deploy platform-core 0.1.17

Source              abc123def456
Current production  0.1.16 (89123ab)
Proposed            0.1.17 (abc123d)

Changes since production
  #118 payment retry hardening
  #121 classroom reconnect fix

Artifacts
  app               ✓ sha256:...

Verification
  Typecheck         PASS
  Tests             PASS
  CVE scan          PASS

Database
  migration head    041 → 043
  mode              forward-compatible
  rollback safe     yes
```

Diff policy: only `.deploy/releases/<new>.yaml` and
`.deploy/environments/<env>.yaml` may change — anything else fails the
promotion check (see [trust-model.md](trust-model.md)).

**Merge = authorize production.**

## Deployment state machine

```
VALIDATE → PREFLIGHT → STAGE → MIGRATE → APPLY → VERIFY → COMMIT STATE
```

The toolkit owns the state machine; the project's hooks supply the commands
(see [consumer-contract-v1.md](consumer-contract-v1.md)).

Every stage's output is recorded in the deployment history
(`history.jsonl` on the target) and reported as a GitHub Deployment status —
an audit/UI projection, never the source of truth.

### Failure handling

```
failure
   │
   ├── rollbackSafe = true  and env policy permits
   │       → rollback
   │       → verify previous release
   │
   └── otherwise
           → STOP + FAIL LOUDLY
```

`migration.rollbackSafe: false` or `failurePolicy.autoRollback: off` always
wins: no silent undo of an unsafe state.

## Rollback

**Normal** rollback is the same promotion machinery with a reverse diff:

```diff
 spec:
-  release: ../releases/platform-core-0.1.17.yaml
+  release: ../releases/platform-core-0.1.16.yaml
```

PR title: `deploy: roll production back to 0.1.16`. Human merges; the toolkit
applies it. Nothing special.

**Emergency** rollback skips the gate when production is broken:

```
deployctl rollback production --to 0.1.16
```

via an explicit manual workflow that requires typed confirmation. It deploys
the previous known release immediately, leaving Git desired state temporarily
drifted — then automatically opens `deploy: reconcile production after
emergency rollback` to restore the invariant.

## Truth and projections

```
Git desired state          authoritative intent
Server observed state      authoritative observation
GitHub Deployments         audit / UI projection
```

## Implementation sequence

1. ~~Schemas (Project, Release, Environment, Target) + `deployctl validate`~~ **done**
2. Release eligibility + immutable artifact resolution
3. Generated promotion PR flow (+ diff allowlist check)
4. `local` transport (deterministic integration tests)
5. SSH transport (strict host verification)
6. Server-side staging, observed state, history
7. Lifecycle execution + verification
8. Rollback (normal + emergency + reconcile)
9. `platform-core` integration, then `examples/static-site`
