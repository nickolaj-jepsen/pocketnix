{
  # This guest's system, rebuilt on the phone by the pocketnix app.
  # The app owns pocketnix.json (its settings) and the release tag below,
  # which Update moves to the app's own version. Your own changes go in
  # configuration.nix.
  inputs.pocketnix.url = "github:nickolaj-jepsen/pocketnix/v@version@";

  outputs = {pocketnix, ...}: {
    nixosConfigurations.pocketnix = pocketnix.lib.managedSystem {
      selections = builtins.fromJSON (builtins.readFile ./pocketnix.json);
      modules = [./configuration.nix];
    };
  };
}
