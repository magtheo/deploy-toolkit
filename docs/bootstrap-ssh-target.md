# Bootstrapping an SSH Deployment Target

> This guide shows one recommended way to prepare an SSH-reachable Linux host for Deploy Toolkit.
>
> It is an **operational guide**, not part of the Target contract. Provider choice, operating system,
> runtime, firewall policy, and application configuration remain consumer decisions.
>
> For the normative requirements, see `docs/target-prerequisites.md`.

## Scope

Deploy Toolkit deliberately does not provision servers, create operating-system users, install runtimes,
configure firewalls, or manage application secrets. It expects a target that already satisfies the
declared Target contract.

This guide bridges the gap between:

```text
"I have a fresh Linux host"
```

and:

```text
"I have a target that Deploy Toolkit can safely deploy to"
```

It focuses on SSH targets because they are the most common remote target shape. The same principles
apply to other environments: separate human administration from deployment automation, keep persistent
state outside immutable releases, and give the deployment identity only the capabilities required by
the consumer's lifecycle hooks.

---

## Mental model

A recommended target separates **human administration** from **deployment automation**:

```text
human operator
      |
      | personal SSH key
      v
   admin user
      |
      | sudo when required
      v
     root
```

and separately:

```text
CI / Deploy Toolkit
      |
      | deployment SSH key
      v
   deploy user
      |
      v
consumer runtime
```

The deployment account should not be the normal human administration account.

Deploy Toolkit itself does not require root. It connects as the user declared by the Target manifest and
executes the deployment lifecycle as that identity.

The deployment identity therefore needs:

- SSH access;
- write access to the declared `deployRoot`;
- access to the tools invoked by the consumer's lifecycle hooks;
- access to private artifacts if the consumer uses them.

It does **not** need administrative privileges merely because it is a deployment account.

---

## 1. Establish human administration

Before creating an automation identity, make sure the machine has a recoverable human administration path.

A common model is:

```text
admin
  - personal SSH key
  - sudo
  - password or other local authentication for sudo
  - not used by CI
```

For example (Debian/Ubuntu; `adduser` and the `sudo` group are distribution-specific —
other distributions use different tools and group names, e.g. `useradd` or a `wheel` group):

```bash
sudo adduser admin
sudo usermod -aG sudo admin
```

Install the operator's public SSH key in:

```text
/home/admin/.ssh/authorized_keys
```

with restrictive permissions:

```bash
sudo install -d -m 700 -o admin -g admin /home/admin/.ssh
sudo chown admin:admin /home/admin/.ssh/authorized_keys
sudo chmod 600 /home/admin/.ssh/authorized_keys
```

Verify from a separate terminal before removing any fallback access:

```bash
ssh admin@target.example
whoami
sudo whoami
```

Expected:

```text
admin
root
```

Keep at least one working administrative session open while changing SSH or firewall policy so a mistake
does not lock you out.

---

## 2. Create the deployment identity

Use a separate operating-system account for Deploy Toolkit.

Example:

```bash
sudo adduser --disabled-password deploy
```

The exact command varies by distribution.

The important properties are:

```text
deploy
  - has SSH key authentication
  - owns or can write deployRoot
  - has no sudo by default
  - has only the runtime permissions required by lifecycle hooks
```

If a password was temporarily assigned during bootstrap, lock it once key authentication is working:

```bash
sudo passwd -l deploy
```

Do not add `deploy` to an administrative group unless the consumer's deployment model has an explicit,
reviewed need for it.

---

## 3. Separate SSH credentials

There are three different key roles that are easy to confuse:

| Credential | Purpose | Where it lives |
| --- | --- | --- |
| Human private key | Authenticates the operator | Operator machine |
| Deployment private key | Authenticates CI / Deploy Toolkit | CI secret store |
| Server host key | Authenticates the target server | Target host; pinned copy available to deployment runner |

The deployment key pair should be distinct from the operator's personal key.

A typical deployment key flow is:

```text
deployment private key
        |
        v
CI secret store

deployment public key
        |
        v
/home/deploy/.ssh/authorized_keys
```

The deployment account's `authorized_keys` should contain only keys that genuinely need deployment access.

A human operator should normally connect using the human admin account instead of sharing the deployment
identity.

---

## 4. Verify and pin the host identity

SSH authenticates in both directions:

- the server authenticates the deployment client using the deployment public key;
- the deployment client authenticates the server using the server's host key.

Deploy Toolkit requires strict host-key verification for SSH targets.

Obtain the target's public host key and verify its fingerprint through a trusted, out-of-band channel such
as:

- the hosting provider console;
- an already-trusted administrative session;
- physical or infrastructure-management access.

Do not blindly trust the output of `ssh-keyscan` for an unverified host.

A pinned host key looks like:

```text
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
```

The Target manifest does not contain the key itself. It names an environment variable whose value points
to a file containing the pinned key.

---

## 5. Prepare `deployRoot`

The Target manifest declares an absolute `deployRoot`.

For example:

```yaml
spec:
  deployRoot: /srv/deploy
```

The deployment identity must be able to create and write the toolkit-owned tree beneath it.

A typical bootstrap:

```bash
sudo mkdir -p /srv/deploy
sudo chown deploy:deploy /srv/deploy
```

Deploy Toolkit derives project-specific state beneath this root, conceptually:

```text
<deployRoot>/
└── <project>/
    ├── releases/
    ├── state/
    ├── history/
    └── ...
```

The exact internal layout is owned by Deploy Toolkit and should not be treated as an application API.

---

## 6. Keep persistent consumer state outside release trees

Release trees are immutable and replaceable. Anything that must survive release replacement must live
outside them.

A useful convention is:

```text
<deployRoot>/<project>/
├── releases/     # toolkit-managed immutable release trees
├── state/        # toolkit-managed deployment state
├── history/      # toolkit-managed deployment history
└── secrets/      # consumer-managed persistent configuration
```

Application state may also live elsewhere, for example in:

- container volumes;
- databases;
- object storage;
- persistent upload directories;
- external services.

The important invariant is:

```text
persistent application state must not depend on an immutable release directory
```

Deploy Toolkit does not read, write, or transport application secrets.

---

## 7. Install consumer runtime prerequisites

Deploy Toolkit does **not** require Docker, Compose, systemd, Node.js, Python, or any other particular
runtime.

The consuming project's lifecycle hooks determine what the target needs.

For example:

```text
apply hook calls: docker compose ...
        |
        v
target needs Docker + Compose
```

or:

```text
apply hook calls: systemctl restart my-service
        |
        v
target needs the appropriate systemd service and permissions
```

Inspect the project's declared hooks before bootstrapping the target.

Install only what the hooks actually require.

---

## 8. Configure runtime privileges carefully

"No sudo" does not necessarily mean "low privilege."

For example, on a conventional Docker host:

```text
membership in the docker group
    ≈
ability to control the host at root-equivalent privilege
```

If a deployment account must run Docker directly, this may be an intentional tradeoff. Document it as
such.

Do not claim a strong privilege boundary merely because the account is absent from `sudoers`.

If a stricter boundary is required, consider a separately designed model such as:

- rootless containers;
- a constrained deployment service;
- narrowly scoped privilege escalation;
- an external orchestrator.

Those are architecture choices and should be introduced deliberately, not as incidental bootstrap steps.

---

## 9. Configure firewall and network access

Deploy Toolkit itself only needs connectivity for the declared transport, typically SSH.

For an SSH target this means: allow the SSH port declared by the Target manifest, normally 22.

```text
deployment runner -> target <declared SSH port, normally 22>
```

Everything else is application-specific.

Examples of consumer-specific network requirements might include:

- HTTP/HTTPS;
- media ports;
- database access;
- VPN connectivity;
- internal service ports.

Do not copy firewall rules from another consumer.

A firewall policy should be derived from the application being deployed.

A sensible default posture is:

```text
incoming: deny by default
outgoing: allow or restrict according to local policy
```

and then explicitly allow only the required ingress.

Always verify a second administrative SSH connection before closing the session used to enable the firewall.

---

## 10. Provision application secrets and configuration

If the consumer expects target-owned configuration, create it before the first deployment.

A common convention is:

```text
<deployRoot>/<project>/secrets/
```

For example:

```bash
sudo install -d -m 700 -o deploy -g deploy /srv/deploy/my-project/secrets
```

This creates and owns only the consumer-managed secrets directory. Do not recursively `chown` the
project tree: `releases/`, `state/`, and `history/` beneath `deployRoot` are toolkit-managed, and
Deploy Toolkit creates its own tree.

A consumer may then place files such as:

```text
/srv/deploy/my-project/secrets/.env
```

with restrictive permissions:

```bash
chmod 600 /srv/deploy/my-project/secrets/.env
```

Secrets must not be:

- committed to Git;
- embedded into release manifests;
- stored inside immutable release trees;
- printed into deployment logs.

Deploy Toolkit does not manage their contents.

---

## 11. Authenticate private artifact registries if required

This is a **consumer/runtime requirement**, not a Deploy Toolkit requirement.

If lifecycle hooks pull private OCI images, authenticate the deployment identity using the runtime's normal
credential mechanism.

For Docker and a private registry, perform the login **as the deployment identity**, not as root or the
admin account — the credential is stored in that account's `~/.docker/config.json`, and a root login
would leave the deployment identity without pull access:

```bash
sudo -u deploy -H docker login registry.example
```

If the consumer only uses public artifacts, no registry login is needed.

Release creation resolves OCI artifacts to immutable digests, but the target still needs permission to pull
private artifacts when the consumer runtime applies the release.

---

## 12. Configure the deployment runner

For SSH targets, the deployment runner needs three pieces of target information:

```text
host
deployment private key
pinned server host key
```

How those three pieces reach the runner depends on which machinery executes the deployment.

**Generic Target contract.** The Target manifest never embeds credentials. It *names environment
variables*, and the deployment process holds the values (for the key material, paths to files):

```yaml
hostFrom:       <env var holding the hostname>
credentialFrom: <env var holding the path to the private-key file>
hostKeyFrom:    <env var holding the path to the pinned host-key file>
```

These variable names are the consumer's choice.

**The published reusable workflow.** Deploy Toolkit's reusable GitHub Actions workflow fixes the
convention on both sides. When using it, the Target manifest must reference exactly:

```yaml
hostFrom:       TOOLKIT_TARGET_HOST
credentialFrom: TOOLKIT_TARGET_SSH_KEY_PATH
hostKeyFrom:    TOOLKIT_TARGET_HOST_KEY_PATH
```

while the workflow caller supplies the values as the workflow's documented secrets:

```text
target_host
target_ssh_key
target_host_key
```

The workflow writes the key material to temporary files and exports the `TOOLKIT_*` variables the
manifest names. The `TOOLKIT_*` names are required only when using that workflow — the generic
Target contract does not mandate them.

In both cases, the Target manifest references environment variables, never embeds credentials
directly.

---

## 13. Verify the target before the first release

Before creating or deploying the first real release, prove that the target satisfies the expected contract.

Useful checks include:

```bash
whoami
id
```

Verify the deployment identity can write the deploy root:

```bash
test -w /srv/deploy
```

Verify consumer runtime dependencies:

```bash
command -v docker
docker compose version
```

or the equivalent for the consumer's actual runtime.

Verify the application secrets/configuration expected by lifecycle hooks exists:

```bash
test -f /srv/deploy/my-project/secrets/.env
```

Verify registry access if private artifacts are used.

Verify the deployment runner can establish SSH using:

- the deployment private key;
- the declared deployment user;
- the pinned host key.

Do not treat "I can SSH manually as an administrator" as proof that the CI deployment identity is correctly
configured.

---

## Target-ready checklist

Before calling an SSH target bootstrapped:

- [ ] A human administrator can access the host independently of CI.
- [ ] The human admin account can perform required operating-system administration.
- [ ] A separate deployment identity exists.
- [ ] The deployment account authenticates with a dedicated deployment key.
- [ ] The deployment account has no unnecessary `sudo` access.
- [ ] Any powerful runtime privileges granted to the deployment account are understood and documented.
- [ ] The deployment user's public key is installed correctly.
- [ ] The target's SSH host key has been verified and pinned.
- [ ] The declared `deployRoot` exists and is writable by the deployment identity.
- [ ] Consumer lifecycle-hook dependencies are installed.
- [ ] Persistent application secrets/configuration exist outside immutable release trees.
- [ ] Private artifact registry access works, if required.
- [ ] Firewall/network policy allows the deployment transport and required application traffic.
- [ ] CI has the target host, deployment private key, and pinned host key.
- [ ] A fresh deployment-identity SSH connection succeeds.
- [ ] A fresh human-admin SSH connection succeeds.

At this point the target is ready for the release/promotion/deployment flow.

---

## What Deploy Toolkit does not do

Bootstrapping a target must not silently expand Deploy Toolkit's responsibilities.

Deploy Toolkit does **not**:

- choose or create a hosting provider account;
- create virtual machines;
- require a particular Linux distribution;
- create operating-system users;
- install Docker or any other application runtime;
- configure application firewall ports;
- configure DNS;
- create application secrets;
- authenticate to private registries on behalf of the target;
- decide which privileges the consumer runtime requires.

Those are infrastructure, host-policy, or consumer responsibilities.

Deploy Toolkit's responsibility begins once the declared target satisfies its contract.

---

## Provider-specific and consumer-specific guides

This guide is intentionally generic.

A consuming repository may additionally maintain a concrete bootstrap guide, for example:

```text
docs/deploy/provider-staging-bootstrap.md
```

That guide may specify:

- a particular provider;
- a particular Linux distribution;
- Docker or another runtime;
- exact firewall ports;
- exact DNS records;
- exact application secret locations;
- provider-console recovery procedures.

Those details should not be promoted into the Deploy Toolkit contract merely because one real consumer uses
them.

The relationship should remain:

```text
Deploy Toolkit guide
    |
    | generic target preparation model
    v
consumer/provider guide
    |
    | concrete implementation choices
    v
real deployment target
```

---

## Next step: first release and promotion

Once the target is ready, continue with the normal release flow:

```text
eligible source revision
        |
        v
immutable release
        |
        v
promotion proposal
        |
        v
human authorization
        |
        v
deployment
        |
        v
observed state
```

Target bootstrap should be a one-time host-preparation concern.

Release creation, promotion, deployment, rollback, recovery, and observed-state tracking remain Deploy
Toolkit's normal operational lifecycle.
