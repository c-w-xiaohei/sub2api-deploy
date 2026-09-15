{ releaseLock ? builtins.fromJSON (builtins.readFile ./runtime-release.json) }:
let
  fail = message: throw "sub2api Nix runtime: ${message}";
  isString = value: builtins.typeOf value == "string";
  isNonEmptyString = value: isString value && value != "";
  require = condition: message: if condition then null else fail message;
  supportedRoles = [ "controller" "host-environment" ];
  runtimeContract = builtins.fromJSON (builtins.readFile ./runtime-contract.json);
  _contractSchema = require ((runtimeContract.schemaVersion or null) == 1)
    "unsupported runtime contract schema";
  requiredPathsFor = role:
    builtins.seq _contractSchema
      (runtimeContract.requiredPaths.${role} or (fail "runtime contract has no role '${role}'"));
  supportedSystemsFor = role:
    builtins.seq _contractSchema
      (runtimeContract.supportedSystems.${role} or (fail "runtime contract has no role '${role}'"));
  toolPathsFor = role: system:
    let payload = payloadFor role system;
    in {
      shell = "${payload}/tools/bin/sh";
      coreutils = "${payload}/tools/bin";
    };
  validVersion = value: isNonEmptyString value && builtins.match "v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?" value != null;
  validAssetName = value: isNonEmptyString value && builtins.match "[A-Za-z0-9][A-Za-z0-9._+-]*[.](tar[.]gz|tar[.]xz)" value != null;
  validHash = value: isNonEmptyString value && builtins.match "sha256-[A-Za-z0-9+/]{43}=" value != null;
  validCommit = value: isNonEmptyString value && builtins.match "[0-9a-f]{40}" value != null;
  pathParts = value: builtins.filter isString (builtins.split "/" value);
  safeRelativePath = value: isNonEmptyString value &&
    builtins.match "[A-Za-z0-9._+-]+(/[A-Za-z0-9._+-]+)*" value != null &&
    builtins.all (part: part != "." && part != "..") (pathParts value);
  entryTypeAt = expectedType: root: relative:
    let
      parts = pathParts relative;
      walk = directory: remaining:
        let
          entry = builtins.head remaining;
          rest = builtins.tail remaining;
          kind = (builtins.readDir directory).${entry} or null;
        in if rest == [] then kind == expectedType
           else kind == "directory" && walk (directory + "/${entry}") rest;
    in safeRelativePath relative && walk root parts;
  regularFileAt = entryTypeAt "regular";
  filesInDirectory = root: relative:
    let entries = builtins.readDir (root + "/${relative}");
    in builtins.concatLists (map (name:
      let path = relative + "/" + name; kind = entries.${name};
      in if kind == "regular" then [ path ]
         else if kind == "directory" then filesInDirectory root path
         else fail "plugin payload contains a nonregular entry: '${path}'")
      (builtins.attrNames entries));
  checkArtifact = artifact:
    let
      role = artifact.role or (fail "artifact has no role");
      system = artifact.system or (fail "artifact has no system");
      version = artifact.version or (fail "artifact has no version");
      asset = artifact.asset or (fail "artifact has no asset");
      archiveSha256 = artifact.archiveSha256 or (fail "artifact has no archiveSha256");
      narHash = artifact.narHash or (fail "artifact has no narHash");
      inventorySha256 = artifact.inventorySha256 or (fail "artifact has no inventorySha256");
      projectCommit = artifact.projectCommit or (fail "artifact has no projectCommit");
      requiredPaths = artifact.requiredPaths or (fail "artifact has no requiredPaths");
      url = "https://github.com/c-w-xiaohei/sub2api-deploy/releases/download/${version}/${asset}";
      _role = require (builtins.elem role supportedRoles) "unsupported artifact role '${toString role}'";
      _system = require (builtins.elem system (supportedSystemsFor role))
        "unsupported system '${toString system}' for artifact role '${role}'";
      _version = require (validVersion version) "artifact version must be an immutable vX.Y.Z release tag";
      _asset = require (validAssetName asset) "artifact asset must be one archive filename without a path, query, or fragment";
      _archive = require (validHash archiveSha256) "artifact archiveSha256 must be an SRI SHA-256 hash";
      _nar = require (validHash narHash) "artifact narHash must be an SRI SHA-256 hash";
      _inventoryHash = require (validHash inventorySha256) "artifact inventorySha256 must be an SRI SHA-256 hash";
      _commit = require (validCommit projectCommit) "artifact projectCommit must be a full immutable Git commit SHA";
      _paths = require (builtins.isList requiredPaths && requiredPaths == requiredPathsFor role)
        "artifact requiredPaths does not match the ${role} payload contract";
    in builtins.deepSeq [ _role _system _version _asset _archive _nar _inventoryHash _commit _paths ] (artifact // { inherit url; });
  artifacts = releaseLock.artifacts or (fail "release lock has no artifacts list");
  _schema = require ((releaseLock.schemaVersion or null) == 1) "unsupported release lock schema";
  _artifacts = require (builtins.isList artifacts) "release lock artifacts must be a list";
  checkedArtifacts = builtins.deepSeq [ _schema _artifacts ] (map checkArtifact artifacts);
  artifactFor = role: system:
    let entries = builtins.filter (artifact: artifact.role == role && artifact.system == system) checkedArtifacts;
    in if entries == [] then fail "runtime artifact unavailable for role '${role}' and system '${system}'; publish and lock a compliant GitHub Release payload"
       else if builtins.length entries != 1 then fail "release lock has multiple artifacts for role '${role}' and system '${system}'"
       else builtins.head entries;
  payloadFor = role: system:
    let
      _nixVersion = require (builtins.compareVersions builtins.nixVersion "2.19" >= 0)
        "Nix >= 2.19 is required; enable nix-command, flakes, and fetch-tree";
      artifact = artifactFor role system;
      # fetchTree fetches one immutable, unpacked tree and verifies its NAR hash.
      # archiveSha256 remains external release provenance verified by CI; it
      # is not passed to a second fetch of a potentially changed mutable object.
      payloadTree = builtins.fetchTree { type = "tarball"; inherit (artifact) url narHash; };
      payload = payloadTree.outPath;
      inventoryPath = payload + "/share/sub2api-runtime/inventory.json";
      inventory = if regularFileAt payload "share/sub2api-runtime/inventory.json"
        then builtins.fromJSON (builtins.readFile inventoryPath)
        else fail "payload inventory must be a regular file inside the payload";
      _paths = map (path: require (builtins.pathExists (payload + "/${path}")) "payload misses required path '${path}'") artifact.requiredPaths;
      requiredTools = [ "cp" "mkdir" "chmod" "printf" "sha256sum" "cut" "tr" "rm" ] ++
        (if role == "controller" then [ "dirname" "readlink" "ln" "uname" ] else []);
      _tools = [
        (require (regularFileAt payload "tools/bin/sh") "payload prebuilt shell must be a regular file")
        (require (entryTypeAt "directory" payload "tools/bin") "payload prebuilt coreutils must be a directory")
      ] ++ map (name: require (regularFileAt payload "tools/bin/${name}")
        "payload prebuilt coreutils misses '${name}'") requiredTools;
      _plugins = require (role != "controller" || entryTypeAt "directory" payload "workspace/bin/plugins")
        "controller plugin directory must be inside the immutable payload without symlinks";
      pluginFiles = if role == "controller"
        then builtins.seq _plugins (filesInDirectory payload "workspace/bin/plugins")
        else [];
      _inventory = require (
        (inventory.schemaVersion or null) == 1 &&
        (inventory.role or null) == artifact.role &&
        (inventory.system or null) == artifact.system &&
        (inventory.version or null) == artifact.version &&
        (inventory.projectCommit or null) == artifact.projectCommit
      ) "payload inventory does not match the locked artifact identity";
      _inventoryBytes = require (builtins.hashFile "sha256" inventoryPath == builtins.convertHash {
        hash = artifact.inventorySha256; hashAlgo = "sha256"; toHashFormat = "base16";
      }) "payload inventory bytes do not match the locked inventorySha256";
      files = inventory.files or (fail "payload inventory has no files");
      _files = require (builtins.isList files && files != []) "payload inventory files must be a nonempty list";
      checkedFiles = builtins.seq _files (map (file:
        if !builtins.isAttrs file then fail "inventory file must be an object"
        else if !safeRelativePath (file.path or null) then fail "inventory file path must be safe and relative"
        else if !isString (file.sha256 or null) || builtins.match "[0-9a-f]{64}" file.sha256 == null
          then fail "inventory file SHA256 must be lowercase hexadecimal"
        else file) files);
      filePaths = map (file: file.path) checkedFiles;
      _unique = require (builtins.length filePaths == builtins.length (builtins.attrNames
        (builtins.listToAttrs (map (path: { name = path; value = true; }) filePaths))))
        "payload inventory contains duplicate file paths";
      digestRequiredPaths = builtins.filter
        (path: path != "workspace/bin/plugins" && path != "share/sub2api-runtime/inventory.json")
        artifact.requiredPaths;
      _coverage = require (builtins.all (path: builtins.elem path filePaths) digestRequiredPaths)
        "payload inventory does not cover every required runtime file";
      _pluginCoverage = require (role != "controller" ||
        (pluginFiles != [] && builtins.all (path: builtins.elem path filePaths) pluginFiles))
        "controller plugins must be nonempty and fully covered by the file inventory";
      _fileDigests = map (file: require (
        regularFileAt payload file.path &&
        builtins.hashFile "sha256" (payload + "/${file.path}") == file.sha256
      ) "payload file is not regular or its digest mismatches: '${file.path}'") checkedFiles;
    in builtins.seq _nixVersion (builtins.deepSeq [ _paths _tools _plugins _inventory _inventoryBytes _unique _coverage _pluginCoverage _fileDigests ] payload);
in {
  inherit artifactFor payloadFor requiredPathsFor supportedSystemsFor toolPathsFor;
}
