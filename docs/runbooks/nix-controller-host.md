# Nix Controller And Host Operations

This runbook covers a new Controller, new Ubuntu production Hosts, the first
deployment, normal operation, reboot recovery, and upgrades. It applies to the
Pulumi SSH Controller release line, not the legacy per-VPS deployment flow.

## 1. Responsibility Map

| Owner | Responsibility |
| --- | --- |
| Nix | Install exact tool versions and keep immutable Controller/Host environments. |
| `sub2api-deploy` | Validate environment input, decrypt SOPS secrets, and invoke constrained Pulumi operations. |
| Pulumi | Own resource graph, state, dependency order, checkpoints, update lock, retry, and history. |
| Host Provider | Probe the machine and invoke the exact `sub2api-host` in the activated root Nix profile over system OpenSSH. |
| `sub2api-host` | Reconcile Docker, nftables, Traefik, Apps, and local PostgreSQL/Redis on one Host, then exit. |
| systemd | Start the Nix-owned Docker socket/service after Host activation. |

Nix does not decide placement, images, DNS, data ownership, or whether an
update should run. Those decisions remain in environment input and Pulumi.

## 2. Supported Machines

Controller:

- `x86_64-linux`
- official multi-user Nix
- access to the selected Pulumi backend
- SOPS decryption identity
- OpenSSH configuration and verified Host keys
- GitHub CLI (`gh`) authenticated for public release downloads, or an
  organization-approved equivalent downloader

Production Host:

- Ubuntu 24.04 with systemd
- `x86_64` or `aarch64`
- official multi-user Nix
- system `/bin/sh` and standard Ubuntu base utilities
- inbound SSH for the configured alias
- root or non-interactive `sudo -n` for the two fixed Provider entrypoints:
  probe and Host stdio
- no conflicting distro Docker unit, `/run/docker.sock`, `/etc/docker` content,
  or `/var/lib/docker` ownership

Do not install this Host environment over an existing unmanaged Docker setup.
Activation deliberately refuses ambiguous ownership.

## 3. Obtain And Verify A Release

Set the release tag once on the Controller. Replace `vX.Y.Z` with the selected
published version.

```bash
export SUB2API_DEPLOY_VERSION=vX.Y.Z
mkdir -p "$HOME/Downloads/sub2api-deploy-$SUB2API_DEPLOY_VERSION"
cd "$HOME/Downloads/sub2api-deploy-$SUB2API_DEPLOY_VERSION"

gh release download "$SUB2API_DEPLOY_VERSION" \
  --repo c-w-xiaohei/sub2api-deploy \
  --pattern "nix-runtime-descriptor-$SUB2API_DEPLOY_VERSION.tar.gz" \
  --pattern "nix-runtime-descriptor-$SUB2API_DEPLOY_VERSION.tar.gz.sha256"

sha256sum --check "nix-runtime-descriptor-$SUB2API_DEPLOY_VERSION.tar.gz.sha256"
tar -xzf "nix-runtime-descriptor-$SUB2API_DEPLOY_VERSION.tar.gz"
cd descriptor
```

The descriptor pins:

- the exact-SHA project candidate and its unpacked NAR hash;
- the nixpkgs revision;
- Pulumi 3.256.0;
- Cloudflare Provider 6.18.0;
- Upstash Provider 0.5.0.

Keep the verified descriptor directory. The same bytes should be transferred to
each production Host rather than independently selecting a version there.

## 4. New Controller

### 4.1 Install Nix

Install official multi-user Nix using the organization-approved machine image
or the official Nix installation instructions. Start a fresh login shell and
verify:

```bash
nix --version
nix store ping --store daemon
```

The release was verified with Nix 2.35.2. Do not use a source checkout of
Pulumi as a substitute for the packaged CLI. `install-runtime.sh` explicitly
enables the required `nix-command`, `flakes`, and `fetch-tree` features for its
own commands; no global experimental-feature configuration is required.

### 4.2 Install The Controller Environment

From the verified `descriptor` directory:

```bash
./nix/install-runtime.sh controller
hash -r
sub2api-deploy 2>&1 | grep -F 'invalid Pulumi command' || true
pulumi version
sops --version
ssh -V
```

Expected Pulumi version:

```text
v3.256.0
```

`install-runtime.sh` first copies every locked nixpkgs dependency from the
official binary cache. A cache miss fails the installation. It then disables
substituters and builds only the repository-owned wrapper/configuration
derivation; it does not compile the project or a third-party package.

### 4.3 Create A Writable Workspace

```bash
export SUB2API_WORKSPACE="$HOME/sub2api-controller"
mkdir -p "$SUB2API_WORKSPACE"
sub2api-workspace-init "$SUB2API_WORKSPACE"
cd "$SUB2API_WORKSPACE"
```

The command creates immutable links for `Pulumi.yaml` and `bin`. It refuses to
replace conflicting files. Pulumi state, credentials, environment input, and
decrypted temporary data remain outside the Nix store.

### 4.4 Choose And Configure A Pulumi Backend

Choose the backend before the first update. For a single Controller with
separately managed backups, a local backend example is:

```bash
mkdir -p "$HOME/.local/state/sub2api-pulumi"
pulumi login "file://$HOME/.local/state/sub2api-pulumi"
```

For multiple operators or Controller replacement, use an organization-approved
remote Pulumi backend instead. Backend selection, access control, backup, and
recovery are operator responsibilities; Nix does not manage them.

### 4.5 Configure SSH

Create one ordinary OpenSSH alias for each production Host, for example:

```sshconfig
Host production-api-one
    HostName 203.0.113.21
    User deploy
    IdentityFile ~/.ssh/sub2api-production
    IdentitiesOnly yes
```

Verify each Host key through an independent trusted channel before adding it to
`known_hosts`. The Controller uses `StrictHostKeyChecking=yes`, does not update
Host keys, does not prompt for passwords, and does not rewrite SSH config.

Check access:

```bash
ssh -o BatchMode=yes -- production-api-one true
ssh -o BatchMode=yes -- production-api-one 'sudo -n id -u'
```

### 4.6 Configure SOPS

Provision the approved SOPS decryption identity outside the repository. Create:

```text
environments/production/config.yaml
environments/production/secrets.yaml
```

Use the configuration shape documented in the repository `README.md`.
`secrets.yaml` must be SOPS-encrypted and contain the Pulumi passphrase,
revision key, service credentials, and scoped App credentials. Never commit its
plaintext form.

Verify decryption without printing plaintext:

```bash
sops --decrypt environments/production/secrets.yaml >/dev/null
```

## 5. New Production Host

Perform these steps on every production Host before its first Pulumi update.

### 5.1 Install Nix And Transfer The Descriptor

Install official multi-user Nix and verify the daemon as in Controller step
4.1. Transfer the already verified descriptor from the Controller, for example:

```bash
# Run on the Controller.
scp -r "$HOME/Downloads/sub2api-deploy-$SUB2API_DEPLOY_VERSION/descriptor" \
  production-api-one:~/sub2api-nix-descriptor
```

### 5.2 Install And Activate The Host Environment

Run on the production Host:

```bash
cd "$HOME/sub2api-nix-descriptor"
sudo ./nix/install-runtime.sh host-environment

sudo /nix/var/nix/profiles/sub2api-host/bin/sub2api-host-activate --start
```

Activation performs all ownership checks before mutation, installs the owned
Docker unit/socket/configuration, and pins the Host store path at:

```text
/nix/var/nix/profiles/sub2api-host
```

The Provider uses that root profile explicitly for `docker` and `nft`, and
runs its two fixed remote entrypoints through `sudo -n`; it does not depend
on `.profile`, `.bashrc`, the SSH user's ambient PATH, or Docker group access.

Verify:

```bash
readlink -f /nix/var/nix/profiles/sub2api-host
sudo systemctl status sub2api-nix-docker.socket --no-pager
sudo systemctl status sub2api-nix-docker.service --no-pager || true
sudo /nix/var/nix/profiles/sub2api-host/bin/docker version
sudo /nix/var/nix/profiles/sub2api-host/bin/nft --version
sudo test -S /run/docker.sock
```

The Docker service may remain inactive until socket activation receives its
first request. The socket must be active.

Do not install `sub2api-host` separately. The descriptor installation above
places the selected, manifest-verified amd64/arm64 bytes and release metadata in
the Host profile. The Host Provider is limited to probing that profile and
invoking its fixed entrypoint.
The fixed profile path is:

```text
/nix/var/nix/profiles/sub2api-host/bin/sub2api-host
```

## 6. First Deployment

Run from the Controller workspace.

### 6.1 Validate Without Changing Production

```bash
cd "$SUB2API_WORKSPACE"
sub2api-deploy validate production
```

This parses configuration, decrypts SOPS into short-lived memory/files, checks
references, and verifies every SSH alias. It does not perform a Pulumi update.

### 6.2 Preview

```bash
sub2api-deploy pulumi production preview --diff
```

Review the complete Pulumi plan. Confirm Host placement, immutable image
digests, protected data resources, Cloudflare publication, and unexpected
deletes or replacements.

### 6.3 Apply

```bash
sub2api-deploy pulumi production up --yes
```

For each new Host, Pulumi performs a profile probe, reconcile, and final inspect
before dependent publication resources proceed. The profile must have been
installed and activated before `preview` or `up`; do not bypass this with
arbitrary remote commands or a manually selected binary.

### 6.4 Post-Deployment Checks

```bash
sub2api-deploy validate production
ssh -- production-api-one \
  'test -x /nix/var/nix/profiles/sub2api-host/bin/sub2api-host && sudo -n systemctl is-active sub2api-nix-docker.socket'
```

Use the wrapper-supported `validate`, `preview`, and `refresh` checks above for
normal operations. If a separate stack-output inspection is required, run it
interactively with the Controller's explicitly configured Pulumi passphrase;
do not put an unmanaged passphrase into a command, environment, or committed
file.

Also verify application readiness through the operator-approved internal and
public paths. Public smoke testing and business-data correctness remain
operator procedures, not guarantees supplied by this CLI.

## 7. Normal Operations

Validate and preview every change:

```bash
sub2api-deploy validate production
sub2api-deploy pulumi production preview --diff
sub2api-deploy pulumi production up --yes
```

Refresh observed cloud/Host state without applying desired changes:

```bash
sub2api-deploy pulumi production refresh
```

Managed Host import uses the constrained form:

```bash
sub2api-deploy pulumi production import \
  sub2api-host:index:Host host-edge edge
```

Do not use direct `pulumi` commands to bypass the CLI's stack/config/approval
constraints for normal production changes. Follow the repository `README.md`
for staged removal and server retirement; a direct full update is not a safe
substitute for its ordered removal checkpoints.

## 8. Reboot And Cold Start

After a normal production Host reboot:

```bash
sudo systemctl is-enabled sub2api-nix-docker.socket
sudo systemctl status sub2api-nix-docker.socket --no-pager
readlink -f /nix/var/nix/profiles/sub2api-host
test -x /nix/var/nix/profiles/sub2api-host/bin/sub2api-host
sudo -n -- /nix/var/nix/profiles/sub2api-host/bin/sub2api-host probe
```

No Pulumi or Nix reinstall is required. The enabled Docker socket restores the
daemon on demand, the root Nix profile protects the Host environment from
garbage collection, and `sub2api-host` retains its per-Host recovery state in:

```text
/var/lib/sub2api-host
```

After Controller reboot, enter the workspace and validate/preview normally:

```bash
cd "$SUB2API_WORKSPACE"
sub2api-deploy validate production
sub2api-deploy pulumi production preview
```

## 9. Upgrade

1. Download and verify the new descriptor on the Controller.
2. Run `./nix/install-runtime.sh controller` from the new descriptor.
3. Transfer that descriptor to every production Host.
4. On each Host, run `sudo ./nix/install-runtime.sh host-environment` and then:

   ```bash
   sudo /nix/var/nix/profiles/sub2api-host/bin/sub2api-host-activate --restart
   ```

5. On the Controller, run `validate`, then `preview`, then the approved `up`.

The Provider does not upgrade the Host binary during the Pulumi step. The Host
profile is changed only by installing and activating the new descriptor before the
Pulumi operation. Do not garbage-collect the previous Nix profile until the
new Host environment and Docker daemon are verified.

## 10. Maintainer Post-Release Lock Update

The first formal release is intentionally published while the checked-in
`nix/runtime-release.json` has only `{"schemaVersion":2}`. After the release
workflow publishes the exact candidate, the maintainer must promote the
generated lock in a separate commit. This is a repository gate, not a
production Host operation.

Run from a clean maintainer checkout after the release has passed its exact-SHA
CI and Nix gates. The `gh` prerequisite is listed in section 2. Record the
current branch and HEAD before downloading anything; the downloaded lock must
name that same HEAD and the release tag must point to it.

```bash
export SUB2API_DEPLOY_VERSION=v0.2.25
export SUB2API_REPOSITORY=c-w-xiaohei/sub2api-deploy
branch="$(git branch --show-current)"
test -n "$branch"
printf 'lock update branch: %s\n' "$branch"
test -z "$(git status --porcelain --untracked-files=all)"
head="$(git rev-parse HEAD)"
printf 'lock update HEAD: %s\n' "$head"
lock_download="$HOME/Downloads/sub2api-runtime-lock-$SUB2API_DEPLOY_VERSION"
rm -rf "$lock_download"
mkdir -p "$lock_download"
gh release download "$SUB2API_DEPLOY_VERSION" \
  --repo "$SUB2API_REPOSITORY" \
  --pattern runtime-release.json \
  --dir "$lock_download"
project_commit="$(jq -r '.controller.projectCommit // empty' "$lock_download/runtime-release.json")"
[[ "$project_commit" =~ ^[0-9a-f]{40}$ ]]
test "$head" = "$project_commit"
tag_commit="$(gh api "repos/$SUB2API_REPOSITORY/git/ref/tags/$SUB2API_DEPLOY_VERSION" --jq '.object | select(.type == "commit") | .sha')"
test "$tag_commit" = "$project_commit"
if test -f nix/runtime-release.json; then
  if cmp -s "$lock_download/runtime-release.json" nix/runtime-release.json; then
    printf '%s\n' 'checked-in runtime lock already matches the release asset'
    exit 0
  else
    cp -- "$lock_download/runtime-release.json" nix/runtime-release.json
  fi
else
  cp -- "$lock_download/runtime-release.json" nix/runtime-release.json
fi
cmp -- "$lock_download/runtime-release.json" nix/runtime-release.json

nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  eval --impure .#packages.x86_64-linux.controller >/dev/null
nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  eval --impure .#packages.x86_64-linux.host-environment >/dev/null
nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  build --impure --no-link --option max-jobs 1 --option cores 1 \
  .#packages.x86_64-linux.controller
nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  build --impure --no-link --option max-jobs 1 --option cores 1 \
  .#packages.x86_64-linux.host-environment

git diff --check
git diff -- nix/runtime-release.json
git add nix/runtime-release.json
git commit -m 'chore: lock released Nix runtime'
git push origin "$branch"
```

The arm64 formal Host build is covered by the exact descriptor/Nix release
gate. The push starts the post-lock Nix workflow, whose formal-lock job must
evaluate and build the formal Controller and x86 Host; the next CI run verifies
that result. Do not use the checked-in lock for the pre-release local fixture:
before this commit, the Nix workflow must use its explicit local
`hostPayload` seam instead.

## 11. Troubleshooting

Controller diagnostics:

```bash
nix --version
readlink -f "$HOME/.nix-profile"
pulumi version
pulumi whoami
sops --version
ssh -G production-api-one >/dev/null
sub2api-deploy validate production
```

Host diagnostics:

```bash
readlink -f /nix/var/nix/profiles/sub2api-host
sudo systemctl status sub2api-nix-docker.socket sub2api-nix-docker.service --no-pager
sudo journalctl -u sub2api-nix-docker.service --since=-30m --no-pager
sudo /nix/var/nix/profiles/sub2api-host/bin/docker info
sudo /nix/var/nix/profiles/sub2api-host/bin/nft list ruleset
sudo ls -ld /var/lib/sub2api-host /nix/var/nix/profiles/sub2api-host
sudo test -x /nix/var/nix/profiles/sub2api-host/bin/sub2api-host
```

Common failures:

| Failure | Meaning / action |
| --- | --- |
| `controller release is unavailable` | The descriptor does not contain a published `runtime-release.json`; use a formal release descriptor. |
| `nix copy` fails | Required binary-cache content is unavailable. Stop; do not enable source fallback or change lock versions. |
| unknown SSH Host key | Verify the fingerprint independently and update `known_hosts`; never disable strict checking. |
| `sudo -n` fails | Provision the reviewed non-interactive privilege before deployment. |
| activation refuses Docker state | The Host is not clean or ownership evidence is missing. Investigate; do not delete state merely to bypass the check. |
| Host reports Docker/nft executable missing | Confirm `/nix/var/nix/profiles/sub2api-host` points to the activated release, then rerun descriptor installation and activation. |
| recovery-required/conflict | Preserve `/var/lib/sub2api-host` and Pulumi state, inspect the Host journal, and retry the same operation only after resolving the reported precondition. |

## 12. Uninstall Boundary

Do not remove Nix, `/var/lib/docker`, `/var/lib/sub2api-host`, or the installed
Host binary as a routine rollback. Those paths may contain ownership and
recovery evidence or application data. Host retirement must first follow the
Pulumi preserve-data lifecycle. Destructive cleanup requires a separate,
explicitly reviewed operation.
