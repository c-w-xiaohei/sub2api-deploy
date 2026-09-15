{
  description = "Sub2API prebuilt Nix runtime environments";

  outputs = { self }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forSystems = function:
        builtins.listToAttrs (map (system: { name = system; value = function system; }) systems);
      runtime = import ./nix/runtime-lib.nix { };
      environments = import ./nix/environments.nix;
      supports = role: system: builtins.elem system (runtime.supportedSystemsFor role);
      environmentFor = role: system: environments {
        inherit system role;
        payload = runtime.payloadFor role system;
        # These paths are declared by the released payload rather than borrowed
        # from nixpkgs or an ambient builder environment.
        toolPaths = runtime.toolPathsFor role system;
        controllerWorkspaceInit = ./nix/controller-workspace-init.sh;
        hostActivate = ./nix/host-activate.sh;
      };
      runtimeEnvironmentsFor = system:
        (if supports "controller" system then { controller = environmentFor "controller" system; } else { }) //
        (if supports "host-environment" system then { host-environment = environmentFor "host-environment" system; } else { });
    in {
      runtimePaths = forSystems runtimeEnvironmentsFor;
      packages = forSystems runtimeEnvironmentsFor;
      apps = forSystems (system:
        (if supports "controller" system then {
          deploy = { type = "app"; program = "${environmentFor "controller" system}/bin/sub2api-deploy"; };
          workspace-init = { type = "app"; program = "${environmentFor "controller" system}/bin/sub2api-workspace-init"; };
        } else { }) //
        (if supports "host-environment" system then {
          host-activate = { type = "app"; program = "${environmentFor "host-environment" system}/bin/sub2api-host-activate"; };
        } else { }));
    };
}
