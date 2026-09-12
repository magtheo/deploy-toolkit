# Prepared Deployment Artifact v1

Status: **candidate** — follows the Consumer Contract v1 versioning policy; it
freezes only after the first production-shaped rehearsal.

## Purpose

The prepared artifact is the **entire material boundary** between the two
trust stages of a deployment:

```text
PREPARE (repository authority, zero target credential)
    builds the artifact ↓
ARTIFACT BOUNDARY (immutable, verifiable, secret-free)
    ↓
DEPLOY (target credential, zero repository/source access)
```

It exists so that no single process ever holds both repository/source
authority and a production target credential. The deploy side reconstructs
nothing from Git: every byte it needs — and every fact it must verify — is in
the artifact.

## Contents

A prepared artifact is a **directory** with exactly five members:

| Member             | Content                                                          |
| ------------------ | ---------------------------------------------------------------- |
| `prepared.json`    | The integrity manifest (schema `prepared.deployment/v1`)         |
| `release.yaml`     | Exact bytes of the release manifest, as prepared                 |
| `environment.yaml` | Exact bytes of the environment manifest, as prepared             |
| `target.yaml`      | Exact bytes of the target descriptor manifest, as prepared       |
| `bundle.tar`       | Exact canonical deployment bundle bytes (see bundle-format-v1)   |

Manifests are stored **byte-exact**, never re-serialized: what the deploy side
parses is what prepare read from the promoted repository state.

## The integrity manifest

```json
{
  "schema":          "prepared.deployment/v1",
  "project":         "my-app",
  "version":         "1.2.3",
  "environment":     "production",
  "target":          "staging-1",
  "sourceRevision":  "<full 40-hex SHA>",
  "bundleDigest":    "sha256:<hex>",
  "contractDigest":  "sha256:<hex>",
  "digests": {
    "release":      "sha256:<hex of release.yaml bytes>",
    "environment":  "sha256:<hex of environment.yaml bytes>",
    "target":       "sha256:<hex of target.yaml bytes>",
    "bundle":       "sha256:<hex of bundle.tar bytes>"
  }
}
```

Deterministic: fixed key order (Go struct order), no timestamps, no
floating fields. Two prepares of the same repository state produce
byte-identical artifacts.

## The integrity web — what each field binds

Verification on the deploy side is a **closed chain**: every member is bound
by a digest recorded in a different member, and every cross-check must hold
before any target contact.

1. `release.Bundle.Digest` (inside `release.yaml`) binds the bundle bytes.
   This is the load-bearing pin: an attacker cannot alter `bundle.tar`
   without producing new bytes that hash to a value pinned by a release
   manifest that exists in the consumer repository.
2. `release.DeploymentContract.Digest` (inside `release.yaml`) binds the
   deployment contract (`deploy/project.yaml`) inside the bundle; the engine
   re-verifies it against the **staged copy on the target** at deploy time.
3. `digests.release` / `digests.environment` / `digests.target` /
   `digests.bundle` (inside `prepared.json`) bind the exact bytes of each
   member. Swapping or editing any member fails this check.
4. The identity fields (`project`, `version`, `environment`, `target`,
   `sourceRevision`, `bundleDigest`, `contractDigest`) each cross-check
   against the parsed manifests. `prepared.json` cannot claim a different
   environment than `environment.yaml` records, and so on.
5. Cross-manifest invariants: `environment.Spec.Target == target.Metadata.Name`,
   `environment.Spec.Release == .deploy/releases/<project>-<version>.yaml`.

Consequence: to forge a fully self-consistent artifact, an adversary must
produce a bundle whose digest matches the pin inside a release manifest —
i.e. must already possess the exact release material. This phase
deliberately does **not** add signature/key management; artifact *provenance*
between the jobs of one workflow run is carried by the CI system (the deploy
job downloads the artifact the prepare job of the same run uploaded).

## Threat model

| Threat                                             | Defense                                   |
| -------------------------------------------------- | ----------------------------------------- |
| Truncated / corrupted artifact in transit          | Member digests; tar/JSON parse failures   |
| Byte-level tampering with any member               | The integrity web above — fails closed    |
| Substituting another release, environment or target| Identity cross-checks + digests web       |
| Secrets leaking through the artifact               | Target manifest holds env-var **names** only (schema-enforced); prepare refuses any member containing key material; prepare runs with zero credentials |
| Deploy side silently regenerating material from Git| `deploy-prepared` has no `--repo-dir` input at all; verified in tests by deploying with no Git on PATH |
| Swapping the artifact of a different workflow run  | CI artifact scoping; explicit `--environment` expectation flag |

## Verification order (deploy side, fail closed before contact)

1. Parse `prepared.json` strictly (single document, unknown fields rejected,
   schema literal check).
2. All four members present; every member digest matches.
3. Each manifest re-parsed through the one validation pipeline
   (header → JSON Schema → strict decode → semantic invariants) — exactly as
   `deployctl validate` would.
4. Identity and cross-manifest checks (web items 4–5).
5. `sha256(bundle.tar) == release.Bundle.Digest`.
6. Only then: target contact (`connect`), staging, execution.

Any failure is a **refusal** (`outcome: refused`, exit 1): the material does
not verify, nothing was contacted, nothing was executed, and retrying cannot
repair it.

## Rollback

Rollback under the trust split uses **two** prepared artifacts — the
currently deployed (failed) release and the release being restored:

```text
deployctl rollback-prepared --from prepared-current/ --to prepared-previous/
```

Each is fully verified by the same rules; no bundle is fetched from Git. The
artifact design makes rollback a first-class consumer of the same boundary
rather than a hidden source-access path.

## Secrets

The artifact is **non-secret by construction**:

- `target.yaml` names environment variables (`HostFrom`, `CredentialFrom`,
  `HostKeyFrom`); values are resolved only at deploy time from the deploy
  process's environment.
- `prepare` resolves no values and never requires credential env vars to be
  set.
- Prepare refuses to write an artifact whose members contain private-key
  material, as defense in depth beyond the schema.
