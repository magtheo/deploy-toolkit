# Target prerequisites

> **What must exist before Deploy Toolkit can deploy to a target?**
>
> This document is contract reference, not a tutorial. It separates what
> **Deploy Toolkit itself requires** (provider- and runtime-neutral, part
> of the consumer contract) from what **your application and its
> lifecycle hooks require** (consumer choice). Provider-specific
> provisioning — creating a VM, a user, firewall rules — is outside the
> toolkit entirely; a provider is just one way to produce a host that
> satisfies the generic requirements below.

## What Deploy Toolkit requires

These are properties of the Target contract
([`schemas/target.schema.json`](schemas/target.schema.json),
[`templates/target.yaml`](templates/target.yaml)). Nothing here assumes
a particular runtime, registry, or hosting provider.

1. **A reachable target using the declared transport.** `ssh` (default
   port 22) or `local`. For SSH, the network path from wherever
   `deployctl` runs must simply reach the SSH endpoint.

2. **A deployment identity.** The SSH transport declares a `user`; the
   toolkit connects and runs everything as that user. The toolkit never
   needs root, and running deployments as root is a host policy choice
   the contract neither requires nor recommends.

3. **A writable `deployRoot`.** An absolute directory the toolkit owns on
   the target. Everything is derived from it:

   ```text
   <deployRoot>/<project>/releases/<version>/   staged release trees
   <deployRoot>/<project>/state/                observed-state bookkeeping
   <deployRoot>/<project>/history/              deployment history
   ```

   The declared user must be able to create and write this tree.

4. **Authentication material through the Target contract — never in
   Git.** The manifest names environment *variables*; the invoking
   environment holds the values:

   | Manifest field  | Variable's value                                            |
   | --------------- | ----------------------------------------------------------- |
   | `hostFrom`      | the hostname                                                |
   | `credentialFrom`| **path** to the SSH private-key file                        |
   | `hostKeyFrom`   | **path** to a file with the pinned host key                 |

   Note the indirection: `credentialFrom` and `hostKeyFrom` name
   variables whose values are **filesystem paths**, not key material.
   When deploying via the published reusable workflow, the manifest must
   use the workflow's variable names (`TOOLKIT_TARGET_HOST`,
   `TOOLKIT_TARGET_SSH_KEY_PATH`, `TOOLKIT_TARGET_HOST_KEY_PATH`); a
   local `deployctl` invocation may use any names.

5. **A pinned SSH host identity** (SSH targets). The host-key file uses
   the public-key/authorized_keys format (`ssh-ed25519 AAAA… comment`),
   one key per line. Host verification is strict by design; the toolkit
   never disables host-key checking. Capture the key out of band (verify
   the fingerprint through the provider's panel or a first manual
   connection) — never blindly `ssh-keyscan` a host you haven't verified.

6. **The ability to execute the consumer-declared lifecycle hooks.**
   Hooks are argv vectors declared in `project.yaml`. They run on the
   target with the working directory set to the staged release tree
   (`<deployRoot>/<project>/releases/<version>/`) and a **stripped
   environment**: a fixed `PATH` plus `DEPLOY_PROJECT`,
   `DEPLOY_ENVIRONMENT`, `DEPLOY_RELEASE_VERSION`,
   `DEPLOY_SOURCE_REVISION` and `DEPLOY_ARTIFACT_<NAME>`
   (`<image>@<digest>`) per release artifact. See the
   [consumer contract](consumer-contract-v1.md) for hook semantics.
   Whatever interpreters or tools the hooks call (a shell, a container
   runtime, …) must exist on the target — that is a consumer
   requirement, below.

7. **Target-owned state and secrets, outside the release trees.**
   Releases are immutable and replaceable; anything that must survive
   them (application secrets, databases, uploaded data) lives elsewhere
   on the target — conventionally `<deployRoot>/<project>/secrets/`,
   created by your bootstrap and read by your hooks. Deploy Toolkit
   never reads, writes, or transports application secrets.

## What Deploy Toolkit deliberately does NOT require

These are **not** toolkit requirements, whatever individual consumers
happen to use:

- **Docker, Compose, or any container runtime.** The toolkit deploys
  bundles and runs argv hooks; containers are one consumer's choice.
- **A specific artifact registry.** OCI artifacts are resolved to
  immutable digests at release time; any conformant registry works, and
  targets without private-registry artifacts need no registry login at
  all.
- **A hosting provider.** Netcup, Hetzner, DigitalOcean, AWS, bare
  metal, a local VM, Pulumi-managed infrastructure — a provider is just
  how the machine came to exist. The Target manifest describes the
  machine, never its origin.
- **One environment per target.** This is a *consumer* deployment
  decision, not a toolkit constraint. It is a reasonable default (and
  the first consumer chose it deliberately: its fixed Compose project
  name keys volumes and container names), but the toolkit itself is
  indifferent — environments are files under `.deploy/environments/`,
  and multiple environments can point at the same target if the
  consumer's runtime keeps their state separate.
- **Provisioning.** The toolkit never creates machines, users, keys,
  directories (beyond its own layout under `deployRoot`), or firewall
  rules. Produce a host that satisfies the requirements above by
  whatever means you trust.

## Consumer/runtime prerequisites

Determined by **your application and your lifecycle hooks** — document
them next to your hook scripts:

- The **runtime your hooks drive** — e.g. Docker + the Compose plugin if
  your `apply.sh` runs `docker compose`.
- **Registry login on the target** if the hooks pull private artifacts
  (e.g. `docker login ghcr.io` for the deployment user, once, during
  bootstrap).
- **Application secrets and configuration** at a stable location outside
  the release trees (see requirement 7), with correct permissions.
- **Network/firewall configuration the application needs** (public
  ports, DNS) — the toolkit only needs its own SSH path (or local
  exec); everything else is application surface.
- **Tools the hooks call** beyond the fixed `PATH` basics — if a hook
  invokes `docker`, `curl`, or anything else, the target must have it.

## Provider examples

Any provider that can produce a Linux host satisfying the generic
requirements works identically: the same Target manifest, the same
credentials flow, the same deployment. A provider-specific bootstrap
guide (how to create the machine, the user, the key, the firewall on
*that* platform) belongs to the consumer repository or your
infrastructure tooling — it is one example of satisfying this contract,
never part of it.
