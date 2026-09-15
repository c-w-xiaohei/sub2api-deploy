# Nix Runtime Payload Contract

`runtime-release.json` is the only runtime release lock. It deliberately has no
artifact entries until CI publishes compliant GitHub Release payloads. Nix
evaluation of an installable role therefore fails closed with `runtime artifact
unavailable`; it must not be changed to reference legacy VPS bundles.

Each entry has this shape:

```json
{
  "role": "controller",
  "system": "x86_64-linux",
  "version": "vX.Y.Z",
  "asset": "sub2api-runtime-controller-x86_64-linux.tar.gz",
  "archiveSha256": "sha256-...",
  "narHash": "sha256-...",
  "inventorySha256": "sha256-...",
  "requiredPaths": ["..."],
  "projectCommit": "40 lowercase hexadecimal characters"
}
```

`archiveSha256` is raw archive provenance, checked by CI against a GitHub
Release sidecar before the lock is committed. It is not an install-time check.
`narHash` locks the unpacked NAR tree that Nix actually fetches through
`builtins.fetchTree`. `inventorySha256` locks the inventory bytes in that tree;
the inventory file digests are verified against the fetched tree. Nix fetches
once through `fetchTree`; it does not falsely claim that two independent URL
fetches prove identical bytes.

Inventory schema version is `1`. `files` is a nonempty list of
`{ "path": "bin/example", "sha256": "<64 lowercase hexadecimal digits>" }`.
Paths must be unique, relative, and have no `.`/`..` components. Every required
runtime file must be covered (the inventory itself is pinned externally by
`inventorySha256`). Each listed file and its parent directories must resolve
inside the fetched tree without symlinks. The descriptor generator also checks
every executable path declared by `runtime-contract.json` before calculating
the NAR hash. This does not replace CI's ELF closure, plugin-discovery, or
license checks.

Payloads are self-contained, prebuilt runtime closures. They include all ELF
loaders, shared libraries, helper executables, Pulumi components/plugins, and
license/source inventory needed for their role. The required inventory records
the project commit, exact component bytes, upstream versions, ELF dependency
closure, and licenses. Nix downloads and verifies those bytes; it never uses
nixpkgs or project source to compile a runtime, Pulumi, a provider, or any
dependency.

The unpacked fixed-output tree is composed into a small, real derivation for
each role. This is not a source build: its builder uses only the release-locked
prebuilt shell and coreutils paths plus the immutable payload to copy selected
components, install launchers, and generate static configuration. No archive
installer, compiler, source builder, `stdenv`, or `nixpkgs` builder runs. A
missing tools declaration or unavailable release artifact fails closed.

For the current release format, the role payload itself supplies the transitional
prebuilt tool input at fixed required paths: `tools/bin/sh` and
`tools/bin/{cp,mkdir,chmod,printf,sha256sum,cut,tr,rm}` for both roles, plus
`tools/bin/{dirname,readlink,ln,uname}` for the Controller.
`runtime-contract.json` is the only descriptor-level source for required paths,
executable paths, and supported role/system pairs; schemaVersion remains `1`
and no `tools` field is added to `runtime-release.json`. The generated builder
has an empty `PATH` and cannot fall back to host tools. The initial supported
matrix is Controller on `x86_64-linux`, plus Host environment on both Linux
systems; arm64 Controller remains unavailable until it has its own promoted
exact-SHA candidate.

This is not an implemented per-component publishing system. A later release
format may split tools and role payloads into independently published fixed
components, but that requires an explicit descriptor/API migration rather than
silently changing the current lock schema.

The generated Host payload contains `share/sub2api-runtime/activation-manifest`.
It fixes mode, content hash, generated source, and destination for only the
Docker service, socket, and daemon config. `sub2api-host-activate` reads this
manifest and the generated files; it does not construct systemd units or infer
Docker parameters in shell. Ownership/conflict checks, pending recovery, root
profile GC roots, and explicit start/restart behavior remain in activation.

## Descriptor Bundle Files

The descriptor flake is self-contained. The release bundle must include these
files at its root, retaining their relative paths:

```
flake.nix
nix/runtime-contract.json
nix/runtime-lib.nix
nix/environments.nix
nix/host-activate.sh
nix/controller-workspace-init.sh
nix/runtime-release.json
```

`flake.nix` imports only `./nix/...`; no descriptor module uses `../` references
to a deployment checkout. The two production shell files are copied into the
generated role environments rather than duplicated in a role payload. Generated
launchers bind execution to the locked payload `tools/bin/sh` path.

After compliant assets are published, install directly from the flake so Nix
can retain temporary roots until the profile becomes a permanent GC root:

```sh
nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  profile install .#controller
nix --extra-experimental-features 'nix-command flakes fetch-tree' \
  profile install .#host-environment
```

Only the Host environment is currently available on `aarch64-linux`. Do not
split evaluation and installation into two
processes without an explicit GC root. Opaque-path installs do not guarantee
automatic `nix profile upgrade` source tracking: pin a new release, install its
path into a candidate profile, validate it, then explicitly switch/remove the
old entry. Do not silently overwrite a colliding profile entry.

Interface evidence: [Nix installables](https://nix.dev/manual/nix/2.28/command-ref/new-cli/nix#installables)
and [flake opaque path conversion](https://github.com/NixOS/nix/blob/2.28.5/src/libcmd/installable-flake.cc#L82-L101).
These establish API support, not evidence that this project's unpublished
payload has already passed a real install/run test.

Controller payloads preserve the existing release-bundle layout and contain a
launcher at `bin/sub2api-deploy`. The launcher must retain the caller's cwd,
use its sibling prebuilt Pulumi, language host, plugins, SOPS, and SSH, and
keep `PULUMI_HOME`, credentials, staged config, and decrypted files outside the
Nix store. `nix run .#workspace-init -- <workspace>` creates only missing
`Pulumi.yaml` and `bin` symlinks to the immutable payload workspace and rejects
conflicts. It preserves the current CLI's cwd-based `Pulumi.yaml` and relative
`bin/pulumi-program` contract. `nix run .#deploy -- validate production` then
executes the real release launcher from that workspace, rather than replacing
it with a PATH-only wrapper.

Host environment derivations contain `bin/sub2api-host-activate` and generated
activation files from `nix/host-activate.sh` and `nix/environments.nix`. The activation command is
explicit and root-only. It owns only `sub2api-nix-docker.service`,
`sub2api-nix-docker.socket`, `/etc/docker/daemon.json`, and its ownership
manifest. It does not install or replace `sub2api-host`, create Host state,
change SSH, create a deploy user, or grant a general sudo shell.

The activation checks root ownership and non-writability of the production
payload, verifies the root Nix profile tool before mutation, rejects foreign
Docker data, config, units, and sockets, and retains old root profile
generations. A private pending marker makes a failed profile switch re-enterable
without adopting foreign files. `--configure` does not start or restart Docker;
`--start` and `--restart` are explicit maintenance operations. Requires Nix >=
2.19 with `flakes`, `nix-command`, and `fetch-tree` enabled; no `fetch-closure` feature is
used. Existing root bootstrap versus non-root stdio transport remains blocked
pending a fixed-argv privilege design.
