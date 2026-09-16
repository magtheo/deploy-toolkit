# Plan: Promotion-Aware CI Path

> Plan document for issue #10 ("Define a promotion-only CI path that avoids
> rebuilding already-qualified release artifacts"). Substantial feature, so per
> AGENTS.md this plan precedes implementation.
>
> **Problem in one line:** a promotion PR changes only deployment-state
> pointers, yet consumer CI treats it as an application change — re-running
> full qualification and re-publishing artifacts whose discovery tags no
> release will ever resolve. The first consumer rehearsal (staging promotion,
> two-file PR) cost ~40 minutes of CI across the promotion PR and its merge,
> producing evidence the release chain never reads.

## Principles

1. **The toolkit owns the distinction, not the consumer's CI.** No consumer
   runtime (Docker, Node, Caddy, …) appears in toolkit code. The toolkit
   classifies *transitions*; the consumer decides what its qualification is.
2. **Classification is semantic.** The Promotion Diff Policy verdict — a
   complete tree walk — never a path filter. Mixed transitions are invalid
   promotions, not "cheap ordinary changes".
3. **Authority runs from trusted surfaces; cost routing may not.** The
   promotion *gate* (merge authority) is hardened against PR control. The
   *skip routing* (what CI may skip) lives in consumer CI files and is **not
   itself a security authority**: its integrity depends on the required
   governance control over `.github/workflows/**` (below). Without that
   control, a PR can alter its own CI routing, and skipped CI becomes a
   qualification bypass — not a cost defect.
4. **Classifier *result* failures fall closed toward the expensive path**
   (full CI); the *gate* additionally fails closed on INVALID and ERROR.
   Workflow-*machinery* failures (the classify job cannot run at all) fail
   the run and require a rerun: on PRs the enforce job still blocks the
   merge; on pushes, publish is skipped in a red run until rerun. Authority
   never depends on machinery succeeding; machinery failures are visible and
   retried.

## Architecture

One reusable workflow, two trusted surfaces, three frontends over one
semantic engine:

```text
                    ┌──────────────────────────────────────────┐
                    │ shared evaluator (internal/promotion)    │
                    │   diff-policy tree walk (OID+mode+type)  │
                    │   evidence re-verification               │
                    │   freshness rules per mode               │
                    └───────────────┬──────────────────────────┘
                                    │  Classification:
                                    │  PROMOTION | ORDINARY | INVALID | ERROR
          ┌─────────────────────────┼─────────────────────────────┐
          ▼                         ▼                             ▼
 promotion check           promotion classify            promotion classify
 (exists; unchanged)       --mode pr   (via event)       --mode push
 strict gate for           PR routing + gate             push transition
 propose'd PRs                                           before → after
```

### Consumer repository shape (two workflow files)

```text
.github/workflows/
├── ci.yml                 # pull_request + push(main): app qualification,
│                          # artifact publish, cost-routing classify job
└── promotion-gate.yml     # pull_request_target ONLY: the trusted authority gate
```

```yaml
# ci.yml (excerpt) — cost routing inside the consumer's own workflow
on:
  pull_request:
  push:
    branches: [main]

jobs:
  classify:
    uses: magtheo/deploy-toolkit/.github/workflows/promotion.yml@<full-sha>
  test:                      # every app job, unchanged definition
    needs: classify
    if: needs.classify.outputs.promotion_only != 'true'
    ...
  publish:                   # main-only SHA-tagged artifact publication
    needs: classify
    if: github.event_name == 'push' && needs.classify.outputs.promotion_only != 'true'
    ...
```

```yaml
# promotion-gate.yml — trusted authority surface
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

The required check for branch protection is the **`gate` (enforce) job**, not
the classifier.

**Why the classify jobs differ in trust.** The gate workflow's definition is
always taken from the **base branch** (`pull_request_target`), so a PR cannot
alter the pin, the conditions, or the output mapping. The routing workflow
inside `ci.yml`, by contrast, **is not itself a security authority**. Its
integrity depends on the documented required governance control over
`.github/workflows/**`: without that control, a PR can alter its own CI
routing — and skipped CI is then a qualification bypass, not a cost defect.
With the control, altering routing requires owner review — the same authority
merging requires. This is part of the documented integration, not optional
hardening:

> **Required consumer control:** `.github/workflows/**` must be covered by
> CODEOWNERS (or a ruleset) so any PR touching CI definitions requires owner
> review — the same authority that merging requires. This is part of the
> documented integration, not optional hardening.

Future option (out of scope): org rulesets / required workflows owning the
classifier surface.

## The transition model

### PR mode (existing `promotion check`, unchanged)

CAS semantics: base must equal the live trusted head; head must be exactly one
commit on it; full diff-policy walk; evidence re-verification (new-release
case needs a checkout of the pinned source revision — without it, fail
closed). This stays the named strict gate for `promotion propose` output: it
evaluates **known promotion attempts** and keeps its strict behavior.

The classifier's pr mode shares the engine but **runs intent detection first**
(below): an ordinary source PR with many commits or an out-of-date base is
ordinary work — it must never become INVALID for failing rules that only
promotion attempts are subject to.

### Push mode (new): classify the ref transition

The original draft used `diff(S^, S)`; that is topology-fragile (rebase
merges, multi-commit pushes). Instead classify **the state transition of the
trusted ref**, using the identities GitHub already provides:

```text
deployctl promotion classify --mode push \
  --repo OWNER/REPO --base <push.before> --head <push.after>
```

Semantics and hardening:

- `diff(before → after)` must satisfy the Promotion Diff Policy — the same
  tree walk (OID + mode + type per leaf), the same two-entry rule, the same
  environment semantic-equality rule.
- The command **re-resolves the live trusted-branch head itself** and requires
  `after == live head`. Event payloads are inputs, never authority.
- `before` must be an ancestor of `after`: **a force push never classifies as
  PROMOTION**, even with a promotion-shaped content diff.
- `before` or `after` all-zeros (branch create/delete) → not PROMOTION.
- New-release case: the added release file's evidence is re-verified exactly
  as in `promotion check` (checkout of the release's pinned source revision
  required; absent → fail closed).
- Rollback case: no added file; environment flips to an existing immutable
  release; the trusted-base copy is the authority.
- **Topology-agnostic by construction**: the content transition decides;
  merge, squash, rebase, and multi-commit pushes all reduce to
  `diff(before, after)`. Provenance remains branch protection's job.
- The push-side hardening checks (live head, ancestry, zero-SHA) apply
  **within promotion-attempt evaluation only** — they are part of what makes
  an attempt PROMOTION vs INVALID, and never touch ORDINARY transitions
  (see the ordering below).

### Promotion intent detection and evaluator ordering

ORDINARY vs INVALID is decided by **promotion intent**, detected
semantically before any promotion rule is applied:

> **Intent predicate:** the diff contains a semantic change to the
> `spec.release` field of a `.deploy/environments/<environment>.yaml` file.

A deployment-state *pointer* transition is the authority-sensitive operation —
nothing else is. Evaluator ordering (both modes):

```text
inspect the semantic transition
  (PR merge diff, or push before → after)
        ↓
no spec.release transition anywhere
        → ORDINARY   (no CAS, no evidence work; topology free)
        ↓
spec.release transition present  → promotion attempt
        ↓
pr mode:   CAS freshness + tree policy + evidence
push mode: live-head + ancestry + zero-SHA checks
           + tree policy + evidence
        ↓
PROMOTION | INVALID
        ↓
read/infrastructure failure at any step
        → ERROR
```

Consequences:

```text
app code only                              → ORDINARY
.deploy/project.yaml policy change         → ORDINARY
deploy scripts, Caddy config               → ORDINARY
environment spec.target/failurePolicy      → ORDINARY (administrative;
                                             branch protection reviews it)
environment file removed                   → ORDINARY (nothing is pointed
                                             anywhere; prepare/deploy fail
                                             closed on absent desired state)
lone release-file addition, no flip        → ORDINARY (inert; evidence is
                                             re-verified when a pointer
                                             later targets it)
source + spec.release flip                 → INVALID
spec.release + unrelated file              → INVALID
malformed release addition + spec.release  → INVALID
valid release + spec.release only          → PROMOTION
environment-only flip to existing release  → PROMOTION (rollback class)
```

### Classification states and observable mapping

Internal model is a `Classification` type; the CLI exposes it through exit
codes plus a human-readable reason (exact code values fixed at
implementation; the observable distinctions the workflow needs are:
promotion vs not, gate-pass vs gate-fail, and a reason string).

| State | Meaning | `promotion_only` | Authority gate (enforce job) | Full CI / publish |
|---|---|---|---|---|
| `PROMOTION` | valid promotion-only transition | `true` | pass | skip |
| `ORDINARY` | ordinary source/administrative change | `false` | pass ("not a promotion") | run |
| `INVALID` | mixed or malformed — e.g. source change + `spec.release` flip | `false` | **fail** with reason | run |
| `ERROR` | cannot classify (API, data, unreadable) | `false` | **fail** (fail closed) | run |

### Failure semantics: the classifier reports; it never rules

`promotion.yml` **always concludes success and always sets its outputs**. The
classify command's nonzero exits (INVALID, ERROR) are captured and *reported*
as `classification` + `reason`; they are never surfaced as a failed job. This
is deliberate GitHub-mechanics hygiene, not style:

> A job that fails inside a `needs` chain causes dependent jobs to be
> **skipped**. A failing routing classifier would therefore silently skip the
> expensive path it exists to fall back to — inverting fail-closed. The
> enforcement decision belongs to the gate's `gate` (enforce) job and, for
> cost, to the plain `!= 'true'` output test — never to the classifier's
> exit code.

If the reusable workflow machinery itself breaks (checkout failure, bad pin,
action error), the classify job fails with outputs absent. Two consequences,
both safe:

- **Authority:** the enforce job runs `if: always()`, sees
  `needs.classify.result != 'success'`, and fails — the required check is
  unsatisfied and the merge is blocked until classification works.
- **Cost:** routing jobs would be skipped for this run — the run is red and a
  rerun is required. On PRs the merge is blocked regardless (authority); on
  pushes, publish is skipped until the rerun. The invariant is deliberately
  narrowed: **classifier *result* failures fall back to full CI;
  workflow-*machinery* failures fail the run and require rerun.** Making even
  machinery failures route to full CI would require `always()`/result-aware
  conditions in every consumer job — complexity the design refuses.

Why INVALID must fail the gate and not merely "run full CI": a boolean model
turns *mixed promotion + source* into a valid way to alter deployment state —
expensive CI would become an alternative authorization route. With the
four-state model, a mixed PR runs full CI (fail closed on qualification) **and**
fails the gate (fail closed on authority); the merge is blocked and the fix is
trivial — split the PR.

**PR/push asymmetry:** on the push side nothing can be blocked (the push
already happened), so INVALID degrades to ORDINARY behavior there — run CI,
run publish. Push content remains branch protection's domain; the push-side
classifier only avoids rewarding a mixed transition with a free publish skip.

## The reusable workflow: `promotion.yml`

Simpler than `deploy.yml` — one job, because **no target credential exists at
all** in this gate:

- **Refuses non-full-SHA invocation** (same guard as `deploy.yml`).
- **Hardcodes `runs-on: ubuntu-latest`** — the never-executes-PR-code property
  must not accidentally land on consumer self-hosted runners with persistent
  Docker access.
- Checks out the caller repository (full fetch, `persist-credentials: false`)
  and the toolkit at `job.workflow_sha`; builds deployctl from that pinned
  commit. PR code is never executed. The only consumer-content read in the
  new-release evidence path is the *release's pinned source revision* — a
  main-qualified SHA: data, not code.
- Mode inferred from the triggering event (`pull_request_target`/`pull_request`
  → pr mode; `push` → push mode), with explicit `base`/`head` overrides.
- Inputs: optional `base`, `head`, `repo_dir` (monorepo parity with
  `deploy.yml`). **Zero secrets** — the implicit `GITHUB_TOKEN` suffices.
- Permissions floor (contract item): `contents: read`, `checks: read`
  (check-run evidence reads; verify the exact fine-grained permission during
  implementation).
- Outputs (contract items): `promotion_only` (`true` only for PROMOTION),
  `classification`, `reason` — **always set, whatever the classify command
  reported, including ERROR**. The reusable workflow's jobs always conclude
  success (see *Failure semantics* above): the classifier reports, the gate
  decides.

## Consumer integration contract (documentation deliverable)

- `promotion-gate.yml` pattern (`pull_request_target`, minimal permissions)
  and the rule: **the gate never checks out the PR head**.
- `ci.yml` conditional-job pattern — **no `paths-ignore`, ever**: a whole
  workflow skipped by filters leaves required checks pending and blocks PRs;
  jobs skipped by `if:` report `Skipped`, which satisfies branch protection
  deterministically. Required-check list: the gate context plus the consumer's
  app contexts; every context is created on every PR.
- CODEOWNERS/ruleset requirement for `.github/workflows/**` (above).
- Branch-protection guidance for the two CI classes.
- **Deliberate property, documented:** promotion merge commits become
  *unreleasable* — publish jobs skip, so the SHA never gets `success` contexts
  and `release create` refuses it. Correct (they are not source revisions).
  This composes with an existing eligibility rule, pinned by test:

  > GitHub accepts `conclusion: skipped` for required checks;
  > `release create` accepts **only `success`**. `skipped` is deliberately
  > ineligible. One asymmetry, two useful consequences: the fast path is safe,
  > and a promotion merge can never masquerade as a qualified source revision.
  > Test both directions.

- Emergency/normal rollback: rollback-as-promotion takes the fast path (and
  needs no source-revision checkout — the already-immutable-release class),
  so the authorization valve is fast precisely when it matters.

## Versioning / contract status

- `promotion.yml` is a **new consumer-contract surface** (§GitHub workflow
  interface). Candidate v1 absorbs it pre-freeze; afterwards the versioning
  policy applies. Its inputs, outputs, permissions floor, `runs-on`, and the
  full-SHA invocation rule are contract items, pinned by a workflow contract
  test (mirror `workflow_contract_test.go`).
- `promotion classify` is a new CLI command **outside** the frozen
  `deployctl.result/v1` enum (that covers deploy/rollback/status/
  recovery-resolve/prepare only). Additive.
- Docs to touch: `docs/consumer-contract-v1.md` (workflow interface + trust
  surfaces), `docs/promotion-diff-policy.md` (replace "planned; not published
  yet"; add push-transition semantics), `README.md` (consumer section), and a
  new consumer CI-integration section (canonical patterns above).

## Safety invariants (issue #10 list, mapped)

| Invariant | How it holds |
|---|---|
| Human merge = authorization | unchanged |
| Verification from trusted code, never PR code | base-version gate; deployctl built from pinned toolkit SHA |
| New releases re-verified against current evidence | same evaluator in all three frontends |
| Stale/modified proposals fail closed | CAS rules unchanged in pr mode |
| Ordinary source changes get full required CI | `promotion_only=false` → all app jobs run |
| Mixed PR never fast-paths | INVALID: full CI **and** failing gate |
| Publication tied to qualified source revisions | promotion merges publish nothing; merge commits unreleasable |
| Deployment consumes the promoted immutable release | untouched |

## Test plan

1. **Shared evaluator** (unit/integration, reusing `promotion check`
   scaffolding): diff-walk edges (mode flips, symlinks, submodule, truncated
   enumeration), two-entry rule, semantic environment equality, both release
   classes; push-mode edges: zero-SHAs, force push (before not ancestor),
   `after ≠ live head`, rebase and multi-commit transitions, squash.
2. **Classification mapping:** PROMOTION/ORDINARY/INVALID/ERROR for
   representative diffs (including source + `spec.release` mixed → INVALID);
   exit codes and reasons. **Intent-ordering cases:** ordinary multi-commit
   PR and out-of-date base → ORDINARY (CAS not applied); lone release-file
   addition without a flip → ORDINARY; `spec.target` change → ORDINARY;
   intent + each hardening failure (stale base, multi-commit head, force
   push, zero-SHA) → INVALID.
3. **Eligibility asymmetry:** required check with conclusion `skipped` fails
   eligibility; `success` passes (pins the unreleasable-merge property).
4. **Workflow contract test:** pin `promotion.yml` inputs/outputs/permissions/
   runs-on/full-SHA guard — including the property that the workflow's jobs
   conclude success with outputs set for INVALID and ERROR.
5. **Failure semantics (contract-level, not implementation detail):**
   - classifier infrastructure ERROR → `promotion_only=false` → full consumer
     CI runs → authority gate fails → merge blocked;
   - INVALID mixed PR → full consumer CI runs → authority gate fails →
     merge blocked;
   - classifier machinery failure (classify job fails, outputs absent) →
     the `if: always()` enforce job still runs and fails → merge blocked.
6. **Live proof (consumer rehearsal):** the skipped-satisfies-protection
   behavior and the gate-blocking-INVALID behavior are demonstrated on a real
   protected branch — not assumed from documentation.

## Sequencing

```text
1. approve this plan
2. shared evaluator + promotion classify (pr & push modes) + tests 1–3
3. promotion.yml + workflow contract test (test 4)
4. docs + contract text
   (optional: v0.1.0-rc.1 pre-release anchor here — readable SHA, not a freeze)
5. platform-core second rehearsal: first fast-path promotion PR
   (proves: skipped satisfies protection; gate blocks INVALID; publish skips)
6. resolve rehearsal friction
7. freeze Consumer Contract v1
8. cut v0.1.0
```

`v0.1.0` deliberately lands **after** the rehearsal: the contract that
includes this feature must be exercised end to end before it freezes — the
same rule that made the original contract candidate.

## Out of scope

- Consumer CI content (jobs, runtimes, check names) — consumer's.
- Deploy triggering (the deploy caller stays dispatch-only / separate
  push-trigger decision; orthogonal, and cheap once promotion-merge CI costs
  seconds).
- Org rulesets / required workflows as the classifier surface (future option).
- `pull_request`-from-fork hardening beyond read-only token sufficiency (the
  classifier only reads; note in docs).

## Open questions (resolve during implementation)

- Exact fine-grained token permission for check-run reads (`checks: read` vs
  metadata-only) — verify empirically in the workflow.
- Exit-code values for the four classification states (must not collide with
  the operational `deployctl.result/v1` exit-code semantics; document as a
  separate, non-frozen surface).
- Whether `promotion classify` warrants a `--json` envelope at v1 — default
  no: the workflow interface is the contract, not the CLI output format.
