# Boots the example guest under QEMU and drives its agent the way the app
# does, from a second machine over the TCP transport: the build sandbox has no
# /dev/vhost-vsock, so vsock itself is only exercised on a phone.
{
  pkgs,
  self,
}: let
  inherit (import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs) snakeOilPrivateKey snakeOilPublicKey;
  cli = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli;
in
  pkgs.testers.runNixOSTest {
    name = "pocketnix-agent";

    nodes.phone = {
      imports = [self.nixosModules.default ../example/configuration.nix];
      pocketnix.guest = {
        avf.enable = false;
        agent.listen = ["vsock:9100" "tcp:0.0.0.0:9100"];
        terminal.profiles = {
          # The size comes first: the CLI resizes on the session's first output.
          probe.command = ''stty size; id -un; read line; stty size; echo "got:$line"'';
          fail.command = "exit 3";
        };
      };
      networking.firewall.allowedTCPPorts = [9100];
      users.users.nixos.openssh.authorizedKeys.keys = [snakeOilPublicKey];
      specialisation.retheme.configuration.pocketnix.guest.terminal.theme.colors.background = pkgs.lib.mkForce "#123456";
    };

    nodes.client = {
      environment.systemPackages = [cli];
    };

    # By IP: the CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch order.
    testScript = {nodes, ...}: ''
      import json

      # Absolute: the systemd-run unit below doesn't get the system PATH.
      A = "${cli}/bin/pocketnix agent --connect tcp:${nodes.phone.networking.primaryIPAddress}:9100"

      start_all()
      phone.wait_for_unit("pocketnix-agent.service")
      phone.wait_for_open_port(9100)
      client.wait_for_unit("multi-user.target")

      with subtest("the control stream delivers the module's config"):
          cfg = json.loads(client.succeed(f"{A} config"))
          assert cfg["hostname"] == "${nodes.phone.networking.hostName}", cfg["hostname"]
          assert cfg["forwards"] == [{"name": "ssh", "guestPort": 22, "hostPort": 2222, "lan": False}], cfg["forwards"]
          names = sorted(p["name"] for p in cfg["terminal"]["profiles"])
          assert names == ["fail", "htop", "probe", "shell"], names
          assert cfg["terminal"]["defaultProfile"] == "shell"
          assert cfg["terminal"]["theme"]["colors"]["background"] == "#1e1e2e"
          assert cfg["terminal"]["extraKeys"][0][0] == {"key": "ESC"}, cfg["terminal"]["extraKeys"]
          client.succeed(f"{A} font /tmp/font.ttf")
          got = client.succeed("sha256sum /tmp/font.ttf").split()[0]
          assert got == cfg["terminal"]["theme"]["fontSha256"], got

      with subtest("a PTY session runs its profile as the user and follows resizes"):
          out = client.succeed(f"{A} exec --profile probe --size 30x100 --resize 50x132 --input $'typed\\r'")
          assert "nixos" in out, out
          assert "30 100" in out, out
          assert "50 132" in out, out
          assert "got:typed" in out, out

      with subtest("a PTY session's exit status reaches the client"):
          status, _ = client.execute(f"{A} exec --profile fail")
          assert status == 3, status

      with subtest("a forward reaches the guest's sshd"):
          client.succeed("install -m 600 ${snakeOilPrivateKey} /tmp/key")
          out = client.succeed(
              "ssh -i /tmp/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "
              f"-o ProxyCommand='{A} forward 22' nixos@phone echo via-forward"
          )
          assert "via-forward" in out, out

      with subtest("a switch reloads the config live, without restarting the agent"):
          pid = phone.succeed("systemctl show -P MainPID pocketnix-agent")
          client.succeed(f"systemd-run --unit=watch sh -c '{A} watch > /tmp/watch'")
          client.wait_until_succeeds("grep -q '#1e1e2e' /tmp/watch", timeout=60)
          phone.succeed("/run/current-system/specialisation/retheme/bin/switch-to-configuration test")
          client.wait_until_succeeds("tail -n1 /tmp/watch | grep -q '#123456'", timeout=60)
          assert phone.succeed("systemctl show -P MainPID pocketnix-agent") == pid

      with subtest("poweroff over the control stream shuts the guest down"):
          client.succeed(f"{A} poweroff")
          phone.wait_for_shutdown()
    '';
  }
