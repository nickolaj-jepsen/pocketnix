{
  # pocketnix's cache, as in its own flake.nix: Nix ignores an input's nixConfig.
  nixConfig = {
    extra-substituters = ["https://pocketnix.cachix.org"];
    extra-trusted-public-keys = ["pocketnix.cachix.org-1:KklXzNwjpGzopowh99lwsL3IeaidLtH+Fw7wniLw9Nw="];
  };

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";
  # Without `inputs.nixpkgs.follows`: pocketnix's own nixpkgs builds the agent, so it comes from its cache.
  inputs.pocketnix.url = "github:nickolaj-jepsen/pocketnix";

  outputs = {
    nixpkgs,
    pocketnix,
    ...
  }: {
    # Keep it networking.hostName: updates.buildHost.installable defaults to nixosConfigurations.<hostName>.
    nixosConfigurations.phone = nixpkgs.lib.nixosSystem {
      modules = [
        pocketnix.nixosModules.default
        ./configuration.nix
        {nixpkgs.hostPlatform = "aarch64-linux";}
      ];
    };
  };
}
