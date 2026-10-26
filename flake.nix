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
    packages = forAll (pkgs: {
      # One Go module builds both: the guest agent and the host-side CLI (provision, ssh proxy).
      pocketnix = pkgs.callPackage ./nix/go.nix {};
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix;
    });

    apps = forAll (pkgs: {
      provision = {
        type = "app";
        program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix}/bin/pocketnix";
      };
    });

    nixosModules.guest = import ./nix/module.nix {inherit self;};
    nixosModules.default = self.nixosModules.guest;

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
