# Nix Runtime Environments

For complete cold-start, daily-use, reboot, upgrade, and troubleshooting
instructions, see [`docs/runbooks/nix-controller-host.md`](../docs/runbooks/nix-controller-host.md).

Nix declares two environments for ordinary Ubuntu hosts. It does not build the
Sub2API project source.

- `controller` combines one exact-SHA CI release candidate with Pulumi 3.256.0,
  SOPS, OpenSSH, and the pinned Cloudflare and Upstash provider binaries.
- `host-environment` provides Docker, containerd, runc, nftables, iptables, and
  OpenSSH from the repository's locked nixpkgs revision.

`flake.lock` fixes nixpkgs. `nix/prebuilt.nix` fixes the three upstream release
trees whose required versions are not supplied by that revision. There is no
project-specific APK unpacker, ELF dependency inventory, or second package
manager.

The installer passes `nix-command flakes fetch-tree` explicitly and uses
no-compiler composition derivations. Operators do not need to enable those
features globally.

## Project Release Lock

`runtime-release.json` identifies only the CI-built project candidate:

```json
{
  "schemaVersion": 2,
  "controller": {
    "version": "vX.Y.Z",
    "projectCommit": "40 lowercase hexadecimal characters",
    "url": "https://github.com/c-w-xiaohei/sub2api-deploy/releases/download/vX.Y.Z/sub2api-controller-<sha>.tar.gz",
    "narHash": "sha256-...",
    "hostRelease": "sub2api-host-controller@sha256:<sha256 of the 40-character project SHA>",
    "hostPayload": {
      "x86_64-linux": {
        "path": "artifacts/sub2api-host/sub2api-host-linux-amd64",
        "sha256": "lowercase candidate-file SHA256"
      },
      "aarch64-linux": {
        "path": "artifacts/sub2api-host/sub2api-host-linux-arm64",
        "sha256": "lowercase candidate-file SHA256"
      }
    }
  }
}
```

The checked-in lock intentionally has no `controller` entry until a release is
promoted. `nix-release-descriptor.py` verifies the candidate archive against CI
metadata, computes its unpacked NAR hash, and writes the release descriptor.

## Installation

The descriptor archive is the installable flake for a release. Its installer
first copies every locked third-party package closure from the official binary
cache; any cache miss stops the install. It then disables substituters and
installs offline, allowing only the repository's small wrapper/configuration
derivation to run:

```sh
./nix/install-runtime.sh controller
./nix/install-runtime.sh host-environment
```

Nix may run the small environment-composition derivation that creates wrappers
and static configuration. It must not compile the Sub2API project or a
third-party package. Missing binary substitutes are an installation failure,
not permission to change versions or fall back to source.

The Controller keeps mutable Pulumi state, credentials, and decrypted data
outside the store. Run `nix run .#workspace-init -- <directory>` once to create
the expected `Pulumi.yaml` and `bin` links in a writable workspace.

The Host package contains the selected candidate `sub2api-host` bytes and exact
release identity. Installation targets
`/nix/var/nix/profiles/sub2api-host`; activation preflight requires the
executable regular binary and regular release metadata before mutation. The
Host package does not convert Ubuntu to NixOS. Run
`sudo <installed-path>/bin/sub2api-host-activate` explicitly to install the
owned Docker unit, socket, and daemon configuration. `--start` and `--restart`
remain explicit operations. Activation does not alter
SSH configuration, or manage application/data state.
