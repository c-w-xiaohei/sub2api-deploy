{
  description = "Sub2API prebuilt Nix runtime environments";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forSystems = function: nixpkgs.lib.genAttrs systems function;
      pkgsFor = system: import nixpkgs { inherit system; };
      runtime = import ./nix/runtime-lib.nix { };
      compose = import ./nix/environments.nix;
      environmentScripts = {
        controllerWorkspaceInit = ./nix/controller-workspace-init.sh;
        hostActivate = ./nix/host-activate.sh;
      };
      environmentsFor = system:
        let pkgs = pkgsFor system;
        in {
           host-environment = compose (environmentScripts // {
             inherit pkgs;
             role = "host-environment";
             hostPayload = runtime.hostPayloadFor system;
           });
        } // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          controller = compose (environmentScripts // {
            inherit pkgs;
            role = "controller";
            projectPayload = runtime.controllerPayload;
          });
        };
      runtimeInputsFor = system:
        let
          pkgs = pkgsFor system;
          packages = import ./nix/runtime-packages.nix { inherit pkgs; };
          strings = values: map builtins.toString values;
        in {
          host-environment = strings (packages.composition ++ packages.host);
        } // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          controller = strings (packages.composition ++ packages.controller);
        };
      runtimeSourcesFor = system:
        let
          candidate = builtins.toString runtime.controllerPayload;
        in if system != "x86_64-linux" then {
          host-environment = [ candidate ];
        } else
          let
            pkgs = pkgsFor system;
            prebuilt = import ./nix/prebuilt.nix { inherit pkgs; };
          in {
            host-environment = [ candidate ];
            controller = map builtins.toString [
              runtime.controllerPayload
              prebuilt.pulumiTree
              prebuilt.cloudflareProvider
              prebuilt.upstashProvider
            ];
          };
    in {
      packages = forSystems environmentsFor;
      runtimeInputs = forSystems runtimeInputsFor;
      runtimeSources = forSystems runtimeSourcesFor;
      apps = forSystems (system:
        let environments = environmentsFor system;
        in {
          host-activate = { type = "app"; program = "${environments.host-environment}/bin/sub2api-host-activate"; };
        } // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          deploy = { type = "app"; program = "${environments.controller}/bin/sub2api-deploy"; };
          workspace-init = { type = "app"; program = "${environments.controller}/bin/sub2api-workspace-init"; };
        });
    };
}
