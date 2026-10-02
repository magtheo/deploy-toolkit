# Promotion-aware CI

Application CI qualifies source. Promotion CI authorizes a transition
between already-qualified immutable release state. One release used to cost
two full application-CI runs that establish nothing new:

```text
promotion PR       → full application CI again      ← redundant
promotion merge    → full main CI + publish again   ← redundant
```

The promotion PR changes exactly `.deploy/releases/<project>-<version>.yaml`
and `spec.release` in one environment file. Nothing about the application is
re-qualified by rebuilding it — and an image published from the promotion
merge SHA carries a discovery tag no release will ever resolve. Worse than
waste: it blurs the two questions the architecture keeps separate.

The reusable **promotion classification workflow**
(`.github/workflows/promotion.yml`) classifies each transition
semantically — by the Promotion Diff Policy verdict, never by file paths —
so consumer CI can route cost without touching authority:

| Classification | `promotion_only` | Authority gate    | Full CI / publish |
| -------------- | ---------------- | ----------------- | ----------------- |
| `PROMOTION`    | `true`           | pass              | skip              |
| `ORDINARY`     | `false`          | pass              | run               |
| `INVALID`      | `false`          | **fail**          | run               |
| `ERROR`        | `false`          | **fail**          | run               |

`INVALID` (a mixed PR: deployment metadata plus any other change) both runs
full CI *and* fails the gate — the merge is blocked, and the fix is to split
the PR. A boolean model would turn mixed PRs into an alternative
authorization route.

## Canonical consumer shape (two workflow files)

```text
.github/workflows/
├── ci.yml                 # pull_request + push(main): application CI with
│                          # a classify job for cost routing
└── promotion-gate.yml     # pull_request_target ONLY: the trusted authority gate
```

### `ci.yml` — cost routing

```yaml
on:
  pull_request:
  push:
    branches: [main]

jobs:
  classify:
    uses: magtheo/deploy-toolkit/.github/workflows/promotion.yml@<full-sha>
  test:                      # every application job, definition unchanged
    needs: classify
    if: ${{ !cancelled() && (needs.classify.result != 'success' || needs.classify.outputs.promotion_only != 'true') }}
    ...
  publish:                   # main-only SHA-tagged artifact publication
    needs: classify
    if: >-
      ${{
        !cancelled() && github.event_name == 'push' &&
        (needs.classify.result != 'success' || needs.classify.outputs.promotion_only != 'true')
      }}
    ...
```

**The conditions must be status-aware — this is load-bearing.** GitHub
treats skipped jobs as satisfied required checks, and GitHub skips jobs
that `need` a failed job unless the condition uses a status function. A
plain `promotion_only != 'true'` condition would therefore let a
*machinery-failed* classify job skip application CI entirely while the
required contexts still report green — a full-CI bypass through
uncertainty. `!cancelled() && (needs.classify.result != 'success' ||
promotion_only != 'true')` closes it: **uncertainty means the expensive
path**, never the skipped path. (`!cancelled()` rather than `always()`
also honors an explicit cancellation; on cancel the contexts stay
unsatisfied and the merge stays blocked.)

Jobs skipped by a *successful* classification report `Skipped`, which
satisfies branch protection deterministically. **Never `paths-ignore`:**
a whole workflow skipped by filters leaves required check contexts
pending and blocks merges.

### `promotion-gate.yml` — authority

```yaml
on:
  pull_request_target:
    branches: [main]
permissions:
  contents: read
  checks: read
jobs:
  classify:
    uses: magtheo/deploy-toolkit/.github/workflows/promotion.yml@<full-sha>
  gate:
    needs: classify
    if: always()             # a broken classifier must never skip the gate
    runs-on: ubuntu-latest
    steps:
      - name: Enforce the classification
        env:
          RESULT: ${{ needs.classify.result }}
          CLASSIFICATION: ${{ needs.classify.outputs.classification }}
          REASON: ${{ needs.classify.outputs.reason }}
        run: |
          case "$RESULT/$CLASSIFICATION" in
            success/PROMOTION)  echo "promotion-only transition"; exit 0 ;;
            success/ORDINARY)   echo "not a promotion; full CI applies"; exit 0 ;;
            *)                  echo "gate fail: $CLASSIFICATION ($RESULT) — $REASON"; exit 1 ;;
          esac
```

The required check for branch protection is the **`gate` job**, not the
classifier. The gate workflow's definition always comes from the base
branch (`pull_request_target`), so a PR cannot alter the pin, the
conditions, or the output mapping — and it never checks out the PR head.

## Failure semantics: uncertainty means the expensive path

The reusable workflow **always concludes success and always sets its
outputs** for reported classifications (`PROMOTION`, `ORDINARY`,
`INVALID`, `ERROR`). Only `PROMOTION` routes past the expensive path;
every other result routes consumer jobs to it via the status-aware
conditions above, and the gate decides authority. A failing classifier
job must never be able to skip the expensive fallback jobs that depend on
it — that failure mode is a full-CI bypass, not a cost defect.

Workflow *machinery* failures (checkout/build errors, unsupported event,
usage error) fail the run with outputs absent: the routing conditions
still send consumer jobs to the expensive path (`result != 'success'`),
and the caller's `if: always()` gate still runs and blocks. Machinery
failures are additionally visible — a red run — and require a rerun for
their classification to count.

```text
classifier result:
    PROMOTION    → expensive path skips
    anything else → expensive path runs

classifier machinery failure:
    → expensive path runs (status-aware routing)
    → authority gate fails/blocks (its result is not success)
    → red run; rerun required for a classification to count
```

## Why the classify jobs differ in trust

The gate's `promotion-gate.yml` runs on `pull_request_target`, so its
definition is always the base branch's. The routing rules inside the
consumer's own `ci.yml` are **not themselves a security authority** —
their integrity depends on the governance control below. Without it, a PR
could alter its own CI routing, and skipped CI becomes a qualification
bypass, not a cost defect.

> **Required consumer control:** `.github/workflows/**` must be covered by
> CODEOWNERS (or a ruleset) so any PR touching CI definitions requires
> owner review — the same authority that merging requires.

## Branch protection

- Required contexts: the `gate` job plus the consumer's application
  contexts. Every context is created on every PR (skipped jobs satisfy
  protection).
- Application jobs protect source merges; the gate protects all merges.
- A mixed (`INVALID`) PR fails the gate regardless of green application
  checks.
- `pull_request` from forks works: classification only reads (implicit
  read-only token, no PR code executed).

## Operational boundaries (v1)

- **Registry credentials.** The classification workflow holds zero
  secrets by design. New-release evidence re-verification resolves the
  release's source-SHA discovery tag from the OCI registry, using the
  runner's ambient credentials — none, in CI. **v1 promotion CI therefore
  requires anonymously readable qualification artifacts** (e.g. public
  GHCR). Consumers with private registries run `promotion check` /
  `promotion classify` from an operator machine with a configured
  keychain for the new-release class; environment-only flips (rollbacks)
  never touch the registry and work everywhere. An optional read-only
  registry credential would be a consumer-contract change, decided
  before any freeze — not an implicit gap.
- **`pull_request_target` policy.** The trusted authority gate depends on
  the `pull_request_target` event. GitHub is moving to **default
  enforcement that blocks `pull_request_target` in public repositories**
  (from November 2, 2026) unless the repository's Actions event policy
  permits it. Public consumers adopting the canonical gate must
  explicitly allow the event in their Actions settings; otherwise the
  gate never runs and required contexts stay pending.
- **Repository shape.** `repo_dir` selects the consumer repository
  checkout that holds `.deploy/` (default `.`). v1 requires `.deploy/`
  at the **Git repository root** — one project per repository; the
  bundle path and the classifier's tree paths are root-anchored by
  design. Project-root scoping inside a shared repository is a future
  contract change, not an undocumented monorepo mode.

## Deliberate consequences

- **Promotion merge commits are unreleasable.** Their publish jobs skip,
  so the SHA never gets `success` contexts — and `release create` accepts
  only `success` (`skipped` is deliberately ineligible). A promotion merge
  can never masquerade as a qualified source revision.
- **Stale push events are never promotions.** Push classification requires
  `after == live trusted head`; if two promotions race, the losing push
  event falls back to full CI. Correct by design — do not remove the
  live-head check to "fix" the redundant build.
- **Rollback takes the fast path.** Environment-only flips to an existing
  immutable release classify as `PROMOTION` without a source-revision
  checkout — the authorization valve is fast precisely when it matters.
- **Release manifests enter trusted main only as part of a valid promotion
  transition.** Lone release-file additions are `INVALID`, so the
  trusted-base fast path is never fed unaudited evidence.

## Trust invariants

- Promotion authorization remains a human PR merge.
- Classification runs from trusted code (deployctl built from the pinned
  toolkit SHA); PR code is never executed.
- New releases are re-verified against current eligibility evidence in
  every frontend.
- Ordinary source changes always receive the consumer's full required CI —
  under every classifier outcome, including machinery failure.
- Artifact publication remains tied to the exact qualified source revision
  recorded in the immutable release manifest.

Design rationale and invariants: [promotion-ci-plan.md](promotion-ci-plan.md).
Policy: [promotion-diff-policy.md](promotion-diff-policy.md). Contract
surface: [consumer-contract-v1.md](consumer-contract-v1.md).
