# Trust Model

Deploy Toolkit's trust boundaries are the product. Every mechanism below exists
because authorization and execution must not be able to corrupt each other.

## The three-stage discipline

Inherited from CI Toolkit's layered model (deterministic gates → advisory AI →
human judgment):

```
1. deterministic eligibility
       ↓
2. human promotion
       ↓
3. deterministic deployment
```

**AI never decides that production changes.** AI may explain a release,
summarize changes, or highlight risk — advisory only, never merge authority,
never deployment authority.

## Promotion PR diff allowlist

A promotion PR may only modify:

```
.deploy/releases/<new-release>.yaml
.deploy/environments/<environment>.yaml
```

Anything else fails the promotion check. Rationale: the authorization PR must
not be able to modify **the machinery executing the deployment** — workflows,
deploy scripts, or application code riding along in the same merge.

## The deployment contract comes from the promoted release

Deploying revision `abc123` uses:

```
.deploy/project.yaml @ abc123
deploy/**            @ abc123
config/**            @ abc123
```

Never `main`'s current state. If it did, an attacker (or an accident) could
pair an old application with a new deployment procedure — a version-skew bug
class. Application and its deployment semantics are one immutable unit.

## Two trust stages in CI

```
prepare — no production secrets
  verify release, checkout promoted SHA, build bundle, verify checksums

deploy  — only the target connection credential
  download bundle, verify digest, SSH, stage/apply/verify, record
```

The deploy job never checks out application source. Application secrets never
enter GitHub; they live on the target, outside the release.

## Strict host verification

The target pins its SSH host key (`hostKeyFrom` in the Target manifest). Deploy
Toolkit verifies:

- **GitHub → server** authentication (the SSH credential), and
- **server → expected identity** (the pinned host key).

`StrictHostKeyChecking=no` and equivalents must never appear — an attacker who
can intercept DNS/network traffic must not be able to receive deployment
credentials.

## Pinned consumption

Consumers reference this repository's workflows and binary by **full commit
SHA**:

```
uses: magtheo/deploy-toolkit/.github/workflows/deploy.yml@<full-sha>
```

Readable release tags (`v0.4.0 → 12ab...`) are documentation above an
immutable SHA, never the pin itself. A mutable ref would let a compromised
toolkit repository push code into every consumer's secret-bearing jobs.

## Release eligibility is deterministic

Before a promotion PR can be generated, the release must pass deterministic
eligibility:

- revision exists and belongs to the permitted branch (`main`);
- all required GitHub checks passed;
- all declared artifacts resolved with pinned digests;
- bundle built and digested;
- manifest validates against the contract schemas.

CI Toolkit's AI review, where present, remains advisory evidence — it gates
nothing here.

## Rollback policy

- Normal rollback: the same promotion machinery, reverse diff, human merge.
- Emergency rollback: explicit manual workflow, immediate deploy, then an
  automatic reconcile PR restoring Git desired state.
- Auto rollback after a failed deploy only when `migration.rollbackSafe` is
  true and the environment's `failurePolicy.autoRollback` permits it;
  otherwise stop and fail loudly.
