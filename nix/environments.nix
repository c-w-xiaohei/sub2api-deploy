{ pkgs, role, projectPayload ? null, hostPayload ? null, controllerWorkspaceInit, hostActivate }:
let
  lib = pkgs.lib;
  isController = role == "controller";
  isHost = role == "host-environment";
  validRole = lib.assertMsg (isController || isHost) "unsupported Sub2API Nix environment role: ${role}";
  validPayload = lib.assertMsg (!isController || projectPayload != null) "controller requires a released project payload";
  validHostPayload = lib.assertMsg
    (!isHost || (builtins.isAttrs hostPayload && builtins.isString (hostPayload.release or null) && builtins.match "^sub2api-host-controller@sha256:[0-9a-f]{64}$" hostPayload.release != null))
    "host-environment requires a valid released Host payload";
  hostRelease = lib.escapeShellArg (if hostPayload == null then "" else hostPayload.release);
  runtimePackages = import ./runtime-packages.nix { inherit pkgs; };
  prebuilt = import ./prebuilt.nix { inherit pkgs; };
  controllerPath = lib.makeBinPath ([ prebuilt.pulumi ] ++ runtimePackages.controller);
  hostPath = lib.makeBinPath runtimePackages.host;
  dockerUnit = ''
    [Unit]
    Description=Sub2API Nix Docker daemon
    After=network-online.target sub2api-nix-docker.socket
    Wants=network-online.target
    Requires=sub2api-nix-docker.socket

    [Service]
    Type=notify
    Environment=PATH=%s/bin
    ExecStart=%s/bin/dockerd --config-file=/etc/docker/daemon.json --data-root=/var/lib/docker -H fd://
    ExecReload=/bin/kill -HUP $MAINPID
    Restart=on-failure
    LimitNOFILE=1048576

    [Install]
    WantedBy=multi-user.target
  '';
  dockerSocket = ''
    [Unit]
    Description=Sub2API Nix Docker socket

    [Socket]
    ListenStream=/run/docker.sock
    SocketMode=0600

    [Install]
    WantedBy=sockets.target
  '';
in assert validRole && validPayload && validHostPayload;
pkgs.stdenvNoCC.mkDerivation {
  name = "sub2api-${role}-${pkgs.stdenv.hostPlatform.system}";
  dontUnpack = true;
  dontPatchELF = true;
  dontPatchShebangs = true;
  dontStrip = true;
  nativeBuildInputs = [ pkgs.makeWrapper ];
  installPhase = if isController then ''
    mkdir -p "$out/bin" "$out/libexec" "$out/workspace/bin/plugins/resource-cloudflare-v6.18.0" \
      "$out/workspace/bin/plugins/resource-upstash-v0.5.0" "$out/workspace/artifacts/sub2api-host" \
      "$out/artifacts/sub2api-host" "$out/scripts/pulumi-plugins"

    cp ${projectPayload}/bin/sub2api-deploy "$out/libexec/sub2api-deploy"
    cp ${projectPayload}/bin/go "$out/libexec/pulumi-go-shim"
    cp ${projectPayload}/bin/pulumi-resource-sub2api-host "$out/libexec/pulumi-resource-sub2api-host"
    cp ${projectPayload}/bin/pulumi-resource-sub2api-host "$out/bin/pulumi-resource-sub2api-host"
    cp ${projectPayload}/bin/pulumi-program "$out/workspace/bin/pulumi-program"
    cp ${projectPayload}/Pulumi.yaml "$out/workspace/Pulumi.yaml"
    cp ${projectPayload}/go.mod "$out/go.mod"
    cp -R ${projectPayload}/artifacts/sub2api-host/. "$out/artifacts/sub2api-host/"
    cp -R ${projectPayload}/artifacts/sub2api-host/. "$out/workspace/artifacts/sub2api-host/"
    cp -R ${projectPayload}/scripts/pulumi-plugins/. "$out/scripts/pulumi-plugins/"
    cp ${prebuilt.cloudflareProvider}/pulumi-resource-cloudflare "$out/workspace/bin/plugins/resource-cloudflare-v6.18.0/pulumi-resource-cloudflare"
    cp ${prebuilt.upstashProvider}/pulumi-resource-upstash "$out/workspace/bin/plugins/resource-upstash-v0.5.0/pulumi-resource-upstash"
    cp ${controllerWorkspaceInit} "$out/libexec/sub2api-workspace-init"
    substituteInPlace "$out/libexec/sub2api-workspace-init" --replace-fail '#!/bin/bash' '#!${pkgs.bash}/bin/bash'
    chmod 0555 "$out/libexec/sub2api-workspace-init"

    makeWrapper "$out/libexec/sub2api-deploy" "$out/bin/sub2api-deploy" \
      --prefix PATH : "${controllerPath}" \
      --set PULUMI_BUNDLE_ROOT "$out" \
      --set PULUMI_PLUGIN_PATH "$out/workspace/bin/plugins"
    makeWrapper "$out/libexec/pulumi-go-shim" "$out/bin/go" \
      --prefix PATH : "${lib.makeBinPath [ pkgs.bash pkgs.coreutils ]}" \
      --set PULUMI_BUNDLE_ROOT "$out"
    makeWrapper ${prebuilt.pulumi}/bin/pulumi "$out/libexec/pulumi" \
      --prefix PATH : "$out/bin:${controllerPath}" \
      --set PULUMI_BUNDLE_ROOT "$out" \
      --set PULUMI_PLUGIN_PATH "$out/workspace/bin/plugins"
    cp "$out/libexec/pulumi" "$out/bin/pulumi"
    makeWrapper ${pkgs.sops}/bin/sops "$out/bin/sops"
    makeWrapper ${pkgs.openssh}/bin/ssh "$out/bin/ssh"
    makeWrapper ${prebuilt.pulumi}/bin/pulumi-language-go "$out/bin/pulumi-language-go" \
      --prefix PATH : "$out/bin:${controllerPath}"
    makeWrapper "$out/libexec/sub2api-workspace-init" "$out/bin/sub2api-workspace-init" \
      --prefix PATH : "${lib.makeBinPath [ pkgs.coreutils ]}"
  '' else ''
    mkdir -p "$out/bin" "$out/libexec" "$out/etc/sub2api-nix-host/files" "$out/share/sub2api-runtime" "$out/share/sub2api-host"
    cp ${hostPayload.path} "$out/libexec/sub2api-host"
    chmod 0555 "$out/libexec/sub2api-host"
    printf '%s' ${hostRelease} > "$out/share/sub2api-host/release"
    chmod 0444 "$out/share/sub2api-host/release"
    makeWrapper "$out/libexec/sub2api-host" "$out/bin/sub2api-host" \
      --prefix PATH : "${hostPath}"
    chmod 0555 "$out/bin/sub2api-host"
    for binary in docker dockerd; do
      makeWrapper ${pkgs.docker}/bin/$binary "$out/bin/$binary" --prefix PATH : "${hostPath}"
    done
    makeWrapper ${pkgs.containerd}/bin/containerd "$out/bin/containerd" --prefix PATH : "${hostPath}"
    makeWrapper ${pkgs.runc}/bin/runc "$out/bin/runc" --prefix PATH : "${hostPath}"
    makeWrapper ${pkgs.nftables}/bin/nft "$out/bin/nft" --prefix PATH : "${hostPath}"
    makeWrapper ${pkgs.iptables}/bin/iptables "$out/bin/iptables" --prefix PATH : "${hostPath}"
    makeWrapper ${pkgs.iptables}/bin/ip6tables "$out/bin/ip6tables" --prefix PATH : "${hostPath}"
    makeWrapper ${pkgs.openssh}/bin/ssh "$out/bin/ssh"
    cp ${hostActivate} "$out/libexec/sub2api-host-activate"
    substituteInPlace "$out/libexec/sub2api-host-activate" --replace-fail '#!/bin/bash' '#!${pkgs.bash}/bin/bash'
    chmod 0555 "$out/libexec/sub2api-host-activate"
    makeWrapper "$out/libexec/sub2api-host-activate" "$out/bin/sub2api-host-activate" \
      --prefix PATH : "${lib.makeBinPath [ pkgs.coreutils pkgs.findutils pkgs.gnugrep pkgs.gawk pkgs.gnused ]}"

    printf '${dockerUnit}' "$out" "$out" > "$out/etc/sub2api-nix-host/files/sub2api-nix-docker.service"
    printf '%s' '${dockerSocket}' > "$out/etc/sub2api-nix-host/files/sub2api-nix-docker.socket"
    printf '{}\n' > "$out/etc/sub2api-nix-host/files/daemon.json"
    for entry in \
      'sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service' \
      'sub2api-nix-docker.socket /etc/systemd/system/sub2api-nix-docker.socket' \
      'daemon.json /etc/docker/daemon.json'; do
      set -- $entry
      hash="$(sha256sum "$out/etc/sub2api-nix-host/files/$1" | cut -d ' ' -f 1)"
      printf '0644 %s etc/sub2api-nix-host/files/%s %s\n' "$hash" "$1" "$2"
    done > "$out/share/sub2api-runtime/activation-manifest"
'';
}
