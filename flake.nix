{
  description = "pocketnix: NixOS as a real host on Android, under AVF (pKVM)";

  # CI's builds: the aarch64 agent, compositor and kernel. templates/default/flake.nix has a copy,
  # since Nix ignores an input's nixConfig.
  nixConfig = {
    extra-substituters = ["https://pocketnix.cachix.org"];
    extra-trusted-public-keys = ["pocketnix.cachix.org-1:KklXzNwjpGzopowh99lwsL3IeaidLtH+Fw7wniLw9Nw="];
  };

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["aarch64-linux" "x86_64-linux"];
    forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    # The Android SDK is unfree, and androidenv has it for x86_64 only.
    androidPkgs = import nixpkgs {
      system = "x86_64-linux";
      config = {
        allowUnfree = true;
        android_sdk.accept_license = true;
      };
    };
  in {
    packages = forAll (pkgs: let
      own = self.packages.${pkgs.stdenv.hostPlatform.system};
    in
      {
        # The guest's agent and pocketnix-guest.
        pocketnix = pkgs.callPackage ./nix/go.nix {};
        pocketnix-cli = pkgs.callPackage ./nix/cli.nix {};
        default = own.pocketnix-cli;
        # On aarch64 it's the guest's gui.package; from x86_64 it builds under binfmt (slow).
        pocketnix-compositor = pkgs.callPackage ./nix/compositor.nix {};
        # The example guest's disk image, assembled by this system's tools.
        example-image = self.nixosConfigurations.example.config.system.build.pocketnixImage.${pkgs.stdenv.hostPlatform.system};
        # What the app downloads on first run: a managed guest the app hasn't configured yet.
        base-image = self.nixosConfigurations.base.config.system.build.pocketnixImage.${pkgs.stdenv.hostPlatform.system};
        options-doc = import ./nix/options-doc.nix {inherit pkgs nixpkgs self;};
      }
      // nixpkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "aarch64-linux") {
        avf-kernel = pkgs.callPackage ./nix/avf-kernel/package.nix {};
      });

    apps =
      nixpkgs.lib.recursiveUpdate (forAll (pkgs: let
        own = self.packages.${pkgs.stdenv.hostPlatform.system};
      in {
        provision = {
          type = "app";
          program = "${pkgs.writeShellScript "provision" ''exec ${own.pocketnix-cli}/bin/pocketnix provision "$@"''}";
          meta.description = "Install the app on a phone over adb and push a guest's disk image";
        };
        boot-example = {
          type = "app";
          program = "${pkgs.callPackage ./nix/qemu-boot.nix {image = own.example-image;}}/bin/boot-example";
          meta.description = "Boot the example guest's disk image under qemu-system-aarch64";
        };
        dev-agent = {
          type = "app";
          program = nixpkgs.lib.getExe (import ./nix/dev-agent.nix {inherit pkgs self;});
          meta.description = "Run the example guest's agent on this machine for the app on the Android emulator";
        };
        options-doc = {
          type = "app";
          program = nixpkgs.lib.getExe (pkgs.writeShellApplication {
            name = "options-doc";
            runtimeInputs = [pkgs.git pkgs.coreutils];
            text = ''install -m 0644 ${own.options-doc} "$(git rev-parse --show-toplevel)/docs/options.md"'';
          });
          meta.description = "Write docs/options.md from the modules' option descriptions";
        };
        # --local finds this system's compositor and pocketnix-gui-bench on PATH.
        bench = {
          type = "app";
          program = "${pkgs.writeShellScript "bench" ''
            PATH=${own.pocketnix-compositor}/bin:$PATH
            exec ${own.pocketnix-cli}/bin/pocketnix bench "$@"
          ''}";
          meta.description = "Benchmark the GUI pipeline on a phone, or on the desktop with --local";
        };
      })) {
        x86_64-linux.gradle = {
          type = "app";
          program = nixpkgs.lib.getExe (import ./nix/gradle.nix {
            pkgs = androidPkgs;
            inherit (self.packages.x86_64-linux) pocketnix pocketnix-compositor;
          });
          meta.description = "Run ./gradlew with the Android SDK and the JVM tests' real agent, compositor and foot";
        };
      };

    # For a consumer's own nixpkgs (e.g. `pocketnix.guest.package = pkgs.pocketnix`); needs Go ≥ go.mod's.
    overlays.default = final: _prev: {
      pocketnix = final.callPackage ./nix/go.nix {};
      pocketnix-cli = final.callPackage ./nix/cli.nix {};
    };

    nixosModules.guest = import ./nix/module.nix {inherit self;};
    nixosModules.default = self.nixosModules.guest;
    # A guest the phone manages (`pocketnix.guest.managed`).
    nixosModules.managed = {
      imports = [self.nixosModules.guest];
      pocketnix.guest = {
        enable = true;
        managed.enable = true;
      };
    };

    # A managed guest's system, as its own flake (nix/managed/flake.nix) builds it:
    # `selections` is the app's pocketnix.json.
    lib.managedSystem = {
      selections,
      modules ? [],
    }:
      nixpkgs.lib.nixosSystem {
        modules =
          [
            self.nixosModules.managed
            {
              nixpkgs.hostPlatform = "aarch64-linux";
              pocketnix.guest.managed.selections = selections;
            }
          ]
          ++ modules;
      };
    # For the desktop the guests update from (`pocketnix.guest.updates.buildHost`).
    nixosModules.buildHost = ./nix/build-host.nix;

    templates.default = {
      path = ./templates/default;
      description = "A pocketnix guest";
      welcomeText = ''
        A pocketnix guest, `nixosConfigurations.phone`. Rename the user and
        the host in `configuration.nix`, then follow docs/getting-started.md
        at https://github.com/nickolaj-jepsen/pocketnix.
      '';
    };

    nixosConfigurations.base = self.lib.managedSystem {
      selections = {};
      modules = [./nix/managed/configuration.nix];
    };

    nixosConfigurations.example = nixpkgs.lib.nixosSystem {
      modules = [
        self.nixosModules.default
        ./example/configuration.nix
        {nixpkgs.hostPlatform = "aarch64-linux";}
      ];
    };

    checks =
      nixpkgs.lib.recursiveUpdate
      (forAll (pkgs: let
        own = self.packages.${pkgs.stdenv.hostPlatform.system};
      in {
        go = pkgs.callPackage ./nix/go.nix {withTests = true;};
        compositor = pkgs.callPackage ./nix/compositor.nix {withTests = true;};
        avf-boot = pkgs.callPackage ./nix/avf-boot-test.nix {image = own.example-image;};
        format = (import ./nix/treefmt.nix {inherit pkgs;}).check self;
        options-doc = pkgs.runCommand "pocketnix-options-doc-check" {} ''
          diff -u ${./docs/options.md} ${own.options-doc} || { echo "docs/options.md is stale: nix run .#options-doc" >&2; exit 1; }
          touch $out
        '';
        # Evaluates a managed guest's seed flake against this checkout, the way
        # the app configures one: its drv path.
        managed = let
          outputs = (import ./nix/managed/flake.nix).outputs {pocketnix = self;};
        in
          pkgs.writeText "pocketnix-managed" (builtins.unsafeDiscardStringContext outputs.nixosConfigurations.pocketnix.config.system.build.toplevel.drvPath);
        # Evaluates the template against this checkout without building it: its systems' drv paths.
        template = let
          template = (import ./templates/default/flake.nix).outputs {
            inherit nixpkgs;
            pocketnix = self;
          };
        in
          pkgs.writeText "pocketnix-template" (builtins.unsafeDiscardStringContext (toString (
            map (c: c.config.system.build.toplevel.drvPath) (builtins.attrValues template.nixosConfigurations)
          )));
      }))
      {
        x86_64-linux.agent = import ./nix/test.nix {
          inherit self;
          pkgs = nixpkgs.legacyPackages.x86_64-linux;
        };
        x86_64-linux.gui = import ./nix/gui-test.nix {
          inherit self;
          pkgs = nixpkgs.legacyPackages.x86_64-linux;
        };
      };

    # `nix develop`, then `./gradlew assembleDebug`.
    devShells.x86_64-linux.default = import ./nix/devshell.nix {pkgs = androidPkgs;};

    formatter = forAll (pkgs: import ./nix/treefmt.nix {inherit pkgs;});
  };
}
