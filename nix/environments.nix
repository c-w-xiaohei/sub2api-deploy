{ system, role, payload, toolPaths, controllerWorkspaceInit, hostActivate }:
let
  fail = message: throw "sub2api Nix environment: ${message}";
  require = condition: message: if condition then null else fail message;
  shell = toolPaths.shell or (fail "toolPaths.shell is required");
  coreutils = toolPaths.coreutils or (fail "toolPaths.coreutils is required");
  requireTool = name: path: require (builtins.isString path && builtins.substring 0 1 path == "/")
    "${name} must be an absolute prebuilt tool path";
  requirePath = path: require (builtins.pathExists (payload + "/${path}"))
    "payload misses selected component '${path}'";
  dockerUnit = ''
    [Unit]
    Description=Sub2API Nix Docker daemon
    After=network-online.target sub2api-nix-docker.socket
    Wants=network-online.target
    Requires=sub2api-nix-docker.socket

    [Service]
    Type=notify
    Environment=PATH=${payload}/bin
    ExecStart=${payload}/bin/dockerd --config-file=/etc/docker/daemon.json --data-root=/var/lib/docker -H fd://
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
  daemonConfig = "{}\n";
  buildScript = builtins.toFile "sub2api-runtime-environment-builder" ''
    set -eu
    ${coreutils}/mkdir -p "$out"
    ${coreutils}/cp -R --preserve=mode,timestamps ${payload}/* "$out/"
    for directory in "$out/bin" "$out/libexec" "$out/workspace"; do
      if [ -d "$directory" ]; then ${coreutils}/chmod u+w "$directory"; fi
    done
    ${if role == "controller" then ''
      ${coreutils}/mkdir -p "$out/libexec"
      ${coreutils}/cp "$out/bin/sub2api-deploy" "$out/libexec/sub2api-deploy"
      ${coreutils}/cp "$out/bin/go" "$out/libexec/pulumi-go-shim"
      ${coreutils}/cp "$out/bin/pulumi" "$out/libexec/pulumi"
      ${coreutils}/cp "$out/bin/pulumi-resource-sub2api-host" "$out/libexec/pulumi-resource-sub2api-host"
      ${coreutils}/rm "$out/bin/sub2api-deploy" "$out/bin/go"
      ${coreutils}/printf '#!${shell}\nPATH=%s/bin\nPULUMI_BUNDLE_ROOT=%s\nPULUMI_PLUGIN_PATH=%s/workspace/bin/plugins\nexport PATH PULUMI_BUNDLE_ROOT PULUMI_PLUGIN_PATH\nexec %s/libexec/sub2api-deploy "$@"\n' "$out" "$out" "$out" "$out" > "$out/bin/sub2api-deploy"
      ${coreutils}/printf '#!${shell}\nPATH=${coreutils}\nPULUMI_BUNDLE_ROOT=%s\nexport PATH PULUMI_BUNDLE_ROOT\nexec ${shell} %s/libexec/pulumi-go-shim "$@"\n' "$out" "$out" > "$out/bin/go"
      ${coreutils}/chmod 0755 "$out/bin/sub2api-deploy" "$out/bin/go" "$out/libexec/sub2api-deploy" "$out/libexec/pulumi-go-shim" "$out/libexec/pulumi" "$out/libexec/pulumi-resource-sub2api-host"
      ${coreutils}/mkdir -p "$out/workspace/artifacts/sub2api-host"
      for artifact in manifest.json sub2api-host-linux-amd64 sub2api-host-linux-arm64; do
        ${coreutils}/ln "$out/artifacts/sub2api-host/$artifact" "$out/workspace/artifacts/sub2api-host/$artifact"
      done
      ${coreutils}/cp ${controllerWorkspaceInit} "$out/libexec/sub2api-workspace-init"
      ${coreutils}/chmod 0755 "$out/libexec/sub2api-workspace-init"
      ${coreutils}/printf '#!${shell}\nPATH=${coreutils}\nexport PATH\nexec ${shell} %s "$@"\n' "$out/libexec/sub2api-workspace-init" > "$out/bin/sub2api-workspace-init"
      ${coreutils}/chmod 0755 "$out/bin/sub2api-workspace-init"
    '' else ''
      for directory in "$out/etc" "$out/etc/sub2api-nix-host" "$out/share" "$out/share/sub2api-runtime"; do
        if [ -d "$directory" ]; then ${coreutils}/chmod u+w "$directory"; fi
      done
      ${coreutils}/mkdir -p "$out/etc/sub2api-nix-host/files" "$out/share/sub2api-runtime" "$out/libexec"
      ${coreutils}/cp ${hostActivate} "$out/libexec/sub2api-host-activate"
      ${coreutils}/chmod 0755 "$out/libexec/sub2api-host-activate"
      ${coreutils}/printf '#!${shell}\nexec ${shell} %s "$@"\n' "$out/libexec/sub2api-host-activate" > "$out/bin/sub2api-host-activate"
      ${coreutils}/chmod 0755 "$out/bin/sub2api-host-activate"
      ${coreutils}/printf '%s' '${dockerUnit}' > "$out/etc/sub2api-nix-host/files/sub2api-nix-docker.service"
      ${coreutils}/printf '%s' '${dockerSocket}' > "$out/etc/sub2api-nix-host/files/sub2api-nix-docker.socket"
      ${coreutils}/printf '%s' '${daemonConfig}' > "$out/etc/sub2api-nix-host/files/daemon.json"
      for entry in \
        'sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service' \
        'sub2api-nix-docker.socket /etc/systemd/system/sub2api-nix-docker.socket' \
        'daemon.json /etc/docker/daemon.json'; do
        set -- $entry
        hash="$(${coreutils}/sha256sum "$out/etc/sub2api-nix-host/files/$1" | ${coreutils}/cut -d ' ' -f 1)"
        ${coreutils}/printf '0644 %s etc/sub2api-nix-host/files/%s %s\n' "$hash" "$1" "$2"
      done > "$out/share/sub2api-runtime/activation-manifest"
    ''}
  '';
  selected = if role == "controller" then [
    "bin/sub2api-deploy"
    "bin/go"
    "bin/pulumi"
    "bin/pulumi-resource-sub2api-host"
    "bin/sops"
    "bin/ssh"
    "artifacts/sub2api-host/manifest.json"
    "artifacts/sub2api-host/sub2api-host-linux-amd64"
    "artifacts/sub2api-host/sub2api-host-linux-arm64"
    "workspace/Pulumi.yaml"
    "workspace/bin"
  ] else [
    "bin/docker"
    "bin/dockerd"
    "bin/containerd"
    "bin/runc"
    "bin/nft"
    "bin/ssh"
  ];
  checks = [
    (require (builtins.elem system [ "x86_64-linux" "aarch64-linux" ]) "unsupported system '${system}'")
    (require (builtins.elem role [ "controller" "host-environment" ]) "unsupported role '${role}'")
    (requireTool "toolPaths.shell" shell)
    (requireTool "toolPaths.coreutils" coreutils)
  ] ++ map requirePath selected ++ [
    (require (role != "controller" || !builtins.pathExists (payload + "/workspace/artifacts"))
      "controller payload must not pre-populate generated workspace artifacts")
  ];
in builtins.deepSeq checks (builtins.derivation {
  name = "sub2api-${role}-${system}";
  inherit system;
  builder = shell;
  args = [ "-e" buildScript ];
  PATH = "";
  preferLocalBuild = true;
  allowSubstitutes = false;
})
