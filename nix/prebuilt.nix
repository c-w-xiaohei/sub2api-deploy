{ pkgs }:
let
  requireX86 = name: value:
    if pkgs.stdenv.hostPlatform.system == "x86_64-linux" then value
    else throw "${name} is currently released only for x86_64-linux";
  pulumiTree = builtins.fetchTree {
    type = "tarball";
    url = "https://github.com/pulumi/pulumi/releases/download/v3.256.0/pulumi-v3.256.0-linux-x64.tar.gz";
    narHash = "sha256-t6NmjvgfA/J11LuYBB/36X+z47NzqdJjcj7vXzLpXso=";
  };
in {
  inherit pulumiTree;
  pulumi = requireX86 "Pulumi 3.256.0" (pkgs.stdenvNoCC.mkDerivation {
    name = "pulumi-3.256.0";
    dontUnpack = true;
    installPhase = ''
      mkdir -p "$out/bin"
      cp ${pulumiTree}/pulumi "$out/bin/pulumi"
      cp ${pulumiTree}/pulumi-language-go "$out/bin/pulumi-language-go"
    '';
  });
  cloudflareProvider = requireX86 "Cloudflare provider 6.18.0" (builtins.fetchTree {
    type = "tarball";
    url = "https://github.com/pulumi/pulumi-cloudflare/releases/download/v6.18.0/pulumi-resource-cloudflare-v6.18.0-linux-amd64.tar.gz";
    narHash = "sha256-yhAuk9tTv1mnyvB7u4dajDN3hEvevarAUQr0vc/4ZKg=";
  });
  upstashProvider = requireX86 "Upstash provider 0.5.0" (builtins.fetchTree {
    type = "tarball";
    url = "https://github.com/upstash/pulumi-upstash/releases/download/v0.5.0/pulumi-resource-upstash-v0.5.0-linux-amd64.tar.gz";
    narHash = "sha256-FF7OT1Wsz4QXAeL5OB+KxYWRRD2qNf/QrOsuNCB+drU=";
  });
}
