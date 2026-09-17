{ releaseLock ? builtins.fromJSON (builtins.readFile ./runtime-release.json), candidateOverride ? null }:
let
  fail = message: throw "sub2api Nix runtime: ${message}";
  artifact = releaseLock.controller or (fail "controller release is unavailable; publish and lock an exact-SHA candidate first");
  validCommit = value: builtins.isString value && builtins.match "[0-9a-f]{40}" value != null;
  validVersion = value: builtins.isString value && builtins.match "v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?" value != null;
  validHash = value: builtins.isString value && builtins.match "sha256-[A-Za-z0-9+/]{43}=" value != null;
  validRawHash = value: builtins.isString value && builtins.match "^[0-9a-f]{64}$" value != null;
  validHostRelease = value: builtins.isString value && builtins.match "^sub2api-host-controller@sha256:[0-9a-f]{64}$" value != null;
  systems = [ "aarch64-linux" "x86_64-linux" ];
  expectedPayload = {
    "x86_64-linux" = { path = "artifacts/sub2api-host/sub2api-host-linux-amd64"; };
    "aarch64-linux" = { path = "artifacts/sub2api-host/sub2api-host-linux-arm64"; };
  };
  validHostPayloadRecord = value:
    builtins.isAttrs value
    && builtins.attrNames value == [ "path" "sha256" ]
    && builtins.isString (value.path or null)
    && validRawHash (value.sha256 or null);
  expectedURL = "https://github.com/c-w-xiaohei/sub2api-deploy/releases/download/${artifact.version or ""}/sub2api-controller-${artifact.projectCommit or ""}.tar.gz";
  expectedHostRelease = "sub2api-host-controller@sha256:${builtins.hashString "sha256" (artifact.projectCommit or "")}";
  checkedArtifact =
    if (releaseLock.schemaVersion or null) != 2 then fail "unsupported release lock schema"
    else if !validVersion (artifact.version or null) then fail "controller version must be an immutable release tag"
    else if !validCommit (artifact.projectCommit or null) then fail "controller projectCommit must be a full Git SHA"
    else if !validHash (artifact.narHash or null) then fail "controller narHash must be an SRI SHA-256"
    else if (artifact.url or null) != expectedURL then fail "controller URL must match version and projectCommit"
    else if !validHostRelease (artifact.hostRelease or null) || artifact.hostRelease != expectedHostRelease then fail "controller hostRelease must bind to projectCommit"
    else if !(builtins.isAttrs (artifact.hostPayload or null)) || builtins.attrNames artifact.hostPayload != systems then fail "controller hostPayload must contain both supported architectures"
    else if !(builtins.all (system: validHostPayloadRecord artifact.hostPayload.${system}) systems) then fail "controller hostPayload records have an invalid schema"
    else artifact;
  candidate =
    if candidateOverride == null then
      (builtins.fetchTree {
        type = "tarball";
        inherit (checkedArtifact) url narHash;
      }).outPath
    else
      let
        overrideText = builtins.toString candidateOverride;
      in
        if builtins.match "^/nix/store/[^/]+$" overrideText == null then
          fail "candidateOverride must be a single direct Nix store path"
        else builtins.storePath overrideText;
  checkedPayload = builtins.mapAttrs
    (system: expected:
      let
        record = checkedArtifact.hostPayload.${system};
        path = "${candidate}/${record.path}";
      in
        if record.path != expected.path then fail "controller hostPayload path is invalid for ${system}"
        else if !validRawHash (record.sha256 or null) then fail "controller hostPayload SHA256 is invalid for ${system}"
        else if builtins.hashFile "sha256" path != record.sha256 then fail "controller hostPayload SHA256 does not match candidate for ${system}"
        else { inherit path; sha256 = record.sha256; }
    ) expectedPayload;
  hostManifest = builtins.fromJSON (builtins.readFile "${candidate}/artifacts/sub2api-host/manifest.json");
  manifestRecords = if builtins.isAttrs hostManifest then {
    "x86_64-linux" = hostManifest."linux-amd64" or null;
    "aarch64-linux" = hostManifest."linux-arm64" or null;
  } else { };
  validManifest =
    builtins.isAttrs hostManifest
    && builtins.attrNames hostManifest == [ "linux-amd64" "linux-arm64" "release" "schemaVersion" ]
    && hostManifest.schemaVersion == 1
    && builtins.isString (hostManifest.release or null)
    && hostManifest.release != ""
    && builtins.all
      (system:
        let record = manifestRecords.${system};
        in builtins.isAttrs record
          && builtins.attrNames record == [ "path" "sha256" "size" ]
          && record.path == builtins.baseNameOf expectedPayload.${system}.path
          && record.sha256 == checkedPayload.${system}.sha256)
      systems;
  hostRelease =
    if !validManifest || hostManifest.release != checkedArtifact.hostRelease then fail "candidate Host manifest does not match controller hostRelease"
    else hostManifest.release;
in {
  controllerArtifact = checkedArtifact;
  controllerPayload = builtins.deepSeq hostRelease candidate;
  hostPayloadFor = system:
    if !(builtins.elem system systems) then fail "unsupported Host system: ${system}"
    else builtins.deepSeq checkedPayload { path = checkedPayload.${system}.path; release = hostRelease; };
}
