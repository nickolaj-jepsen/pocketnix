{
  description = "pocketnix: NixOS as a real host on Android, under AVF (pKVM)";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["aarch64-linux" "x86_64-linux"];
    forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
  in {
    packages = forAll (pkgs: let
      own = self.packages.${pkgs.stdenv.hostPlatform.system};
    in
      {
        # One Go module builds both: the guest agent and the host-side CLI.
        pocketnix = pkgs.callPackage ./nix/go.nix {};
        pocketnix-cli = pkgs.callPackage ./nix/cli.nix {inherit (own) pocketnix;};
        default = own.pocketnix-cli;
        # The example guest's disk image, assembled by this system's tools.
        example-image = self.nixosConfigurations.example.config.system.build.pocketnixImage.${pkgs.stdenv.hostPlatform.system};
      }
      // nixpkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "aarch64-linux") {
        avf-kernel = pkgs.callPackage ./nix/avf-kernel/package.nix {};
      });

    apps = forAll (pkgs: {
      provision = {
        type = "app";
        program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli}/bin/pocketnix";
      };
    });

    nixosModules.guest = import ./nix/module.nix {inherit self;};
    nixosModules.default = self.nixosModules.guest;

    nixosConfigurations.example = nixpkgs.lib.nixosSystem {
      modules = [
        self.nixosModules.default
        ./example/configuration.nix
        {nixpkgs.hostPlatform = "aarch64-linux";}
      ];
    };

    checks.x86_64-linux.agent = import ./nix/test.nix {
      inherit self;
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
    };

    # `nix develop`, then `./gradlew assembleDebug`.
    devShells.x86_64-linux.default = import ./nix/devshell.nix {
      pkgs = import nixpkgs {
        system = "x86_64-linux";
        config = {
          allowUnfree = true;
          android_sdk.accept_license = true;
        };
      };
    };

    formatter = forAll (pkgs: pkgs.alejandra);
  };
}
