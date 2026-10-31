# Boots a managed guest as the base image first starts, before the app has
# configured it (no user, no switches), and drives its agent the way the app
# does: the seed writes the guest's flake, and the agent serves it as managed.
{
  pkgs,
  self,
}: let
  cli = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli;
in
  pkgs.testers.runNixOSTest {
    name = "pocketnix-managed";

    # The module's own default host name, which the seed's flake names.
    nodes.pocketnix = {
      imports = [self.nixosModules.managed ./managed/configuration.nix];
      # Its options manual would rebuild the node for every option's text; the options-doc check renders them.
      documentation.nixos.enable = false;
      pocketnix.guest = {
        avf.enable = false;
        agent.listen = ["tcp:127.0.0.1:9100"];
      };
    };

    testScript = {nodes, ...}: let
      guest = nodes.pocketnix.pocketnix.guest;
      flake = guest.managed.flake;
    in ''
      import json

      A = "${cli}/bin/pocketnix agent --connect tcp:127.0.0.1:9100"

      pocketnix.wait_for_unit("pocketnix-agent.service")
      pocketnix.wait_for_open_port(9100)

      with subtest("the first boot seeds the guest's flake from the module's selections"):
          pocketnix.succeed("test -f ${flake}/configuration.nix")
          pin = pocketnix.succeed("grep -o 'github:nickolaj-jepsen/pocketnix/v[^\"]*' ${flake}/flake.nix").strip()
          assert "@" not in pin, pin
          seeded = json.loads(pocketnix.succeed("cat ${flake}/pocketnix.json"))
          assert seeded == json.loads('${builtins.toJSON guest.managed.selections}'), seeded

      with subtest("the agent serves the flake as managed"):
          cfg = json.loads(pocketnix.succeed(f"{A} config"))
          assert cfg.get("managed", {}).get("flake") == "${flake}", cfg.get("managed")
          sel = json.loads(pocketnix.succeed(f"{A} selections"))
          assert sel["user"] == "" and not any(sel["services"].values()), sel

      with subtest("with no user yet, tabs run as ${guest.user}"):
          out = pocketnix.succeed(f"{A} exec --command 'id -un'")
          assert "${guest.user}" in out.split(), out

      with subtest("written selections reach the flake, and the seed leaves them"):
          sel = json.loads(pocketnix.succeed(f"""{A} selections '{{"user": "alice", "services": {{"ssh": true}}}}'"""))
          assert sel["changed"] and sel["user"] == "alice" and sel["services"] == {"ssh": True}, sel
          pocketnix.succeed("systemctl restart pocketnix-managed-seed.service")
          assert json.loads(pocketnix.succeed("cat ${flake}/pocketnix.json"))["user"] == "alice"

      with subtest("the nix daemon builds under the managed CPU quota, with the system's nixpkgs"):
          assert pocketnix.succeed("systemctl show -P CPUQuotaPerSecUSec nix-daemon.service").strip() != "infinity"
          registry = json.loads(pocketnix.succeed("cat /etc/nix/registry.json"))
          to = {f["from"]["id"]: f["to"] for f in registry["flakes"]}.get("nixpkgs", {})
          assert to.get("rev") == "${nodes.pocketnix.nix.registry.nixpkgs.to.rev}", to
    '';
  }
