# Bundle Format v1

Status: **candidate** — frozen together with Consumer Contract v1 after a real
`platform-core` deployment has exercised it.

The bundle is the runtime payload a deployment stages on the target. Its
construction must be **deterministic**: the same release revision and the same
Project manifest must always produce byte-identical bundle bytes, and therefore
the same `bundle.digest`. Any nondeterminism (current time, uid/gid, working
directory state) would break release immutability and reproducibility.

## Inputs

The bundle is constructed from the **exact Git tree at the release revision** —
never from whatever files happen to exist in a runner's working directory:

```
release revision (full SHA)
   ↓
Git-tracked tree at that revision
   ↓
paths matched by project.yaml bundle.include globs
   + .deploy/project.yaml (always, automatically)
   ↓
paths lexically sorted
   ↓
canonical tar
   ↓
bundle.digest = SHA-256(canonical uncompressed tar bytes)
```

This automatically excludes untracked local state (`.env`, caches, editor
droppings, build leftovers) from deployments.

## Path matching

- `bundle.include` entries are gitignore-style globs matched against
  **repo-root-relative** Git-tracked paths at the release revision.
- `*` matches within one path segment; `**` matches across segments.
- Leading `/`, backslashes, `.`/`..` segments, and absolute paths are rejected
  (schema pattern + semantic checks; see `internal/manifest`).
- A declared include pattern that matches **zero tracked files** is an error:
  includes declare intent, and silent omission would be contract drift.

## Regular files only

Bundle Format v1 carries **regular files only** — no symlinks, no special
entries. A path tracked as a symlink (or any other non-regular Git object)
**fails bundle construction**; vendor the link's content instead.

Rationale: the v1 transport contract has no symlink primitive, so the
staging substrate cannot materialize symlinks on the target. A bundle the
toolkit could build but never stage would be a cross-layer contradiction —
a release that passes `release create`, survives promotion and a human
merge, and then fails deterministically at staging. The contract is
therefore tightened at the earliest possible point, while it is still
candidate. If a real consumer requires symlinks, the alternative is a true
symlink primitive in the transport/target substrate — not approximation by
regular files with different semantics.

Historical note: earlier candidate revisions preserved symlinks
("preserved, not dereferenced"); this was reconciled with the staging
substrate before the contract was frozen.

## Canonical tar rules

- Format: ustar headers only (no GNU extensions, no PAX metadata).
- Entry order: lexically sorted by path, byte-wise.
- Entries: regular files (`typeflag 0`) only; directories are implicit;
  symlinks and special entries fail construction (see above).
- `mtime = 0`, `uid = 0`, `gid = 0`, `uname = ""`, `gname = ""`.
- Mode: `0644` for files, `0755` only for entries tracked with the Git
  executable bit.
- No devices, no sockets, no fifos — Git does not track them; fail closed if
  somehow present.
- Any path that cannot be represented in the chosen ustar format **fails
  bundle construction**; Deploy Toolkit does not silently switch to
  PAX/GNU extensions.

## Digest definitions

```
deploymentContract.digest = SHA-256(exact bytes of .deploy/project.yaml @ source revision)

bundle.digest             = SHA-256(canonical uncompressed tar bytes)
```

Both digests are over the exact byte sequences as committed — no
canonicalization, no line-ending conversion, no reformatting. The release
manifest therefore pins both *what* is deployed (artifacts) and *how* it must
be deployed (contract), as one immutable unit.

## Verification duty

The deploy stage must recompute `bundle.digest` from the downloaded bundle
bytes before staging, and `deploymentContract.digest` from the staged
`project.yaml`, and fail closed on mismatch. Digest equality — not workflow
plumbing — is what makes "the contract comes from the promoted release"
enforceable.
