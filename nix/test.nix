# Boots the example guest under QEMU and drives its agent the way the app
# does, from a second machine over the TCP transport: the build sandbox has no
# /dev/vhost-vsock, so vsock itself is only exercised on a phone.
{
  pkgs,
  self,
}: let
  inherit (import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs) snakeOilPrivateKey snakeOilPublicKey snakeOilEd25519PrivateKey snakeOilEd25519PublicKey;
  cli = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli;
  # The builder's binary cache; a test-only key.
  cacheSecret = pkgs.writeText "cache.sec" "pocketnix-test-builder:cNCiLJRJKtj/NYr6HKMIB0f0JS0K51uY8Boefn15NtUdEVxl8P73M2yibOJqDDCQxM7dSd+4OSUHzjzwmf6O5A==";
  cachePublic = "pocketnix-test-builder:HRFcZfD+9zNsomziagwwkMTO3UnfuDklB8488Jn+juQ=";
in
  pkgs.testers.runNixOSTest {
    name = "pocketnix-agent";

    nodes.phone = {
      nodes,
      extendModules,
      ...
    }: {
      imports = [self.nixosModules.default ../example/configuration.nix];
      pocketnix.guest = {
        avf.enable = false;
        agent.listen = ["vsock:9100" "tcp:0.0.0.0:9100"];
        terminal.profiles = {
          # The size comes first: the CLI resizes on the session's first output.
          probe.command = ''stty size; id -un; read line; stty size; echo "got:$line"'';
          fail.command = "exit 3";
        };
        updates = {
          buildHost = {
            host = "nix@builder";
            # A symlink to a store path is an installable too.
            installable = "/etc/pocketnix-next";
            sshKey = "/etc/pocketnix-test/id_ed25519";
          };
          # By IP: the agent's pure-Go resolver.
          published.url = "http://${nodes.builder.networking.primaryIPAddress}/toplevel";
          checkInterval = null;
        };
      };
      # Updates switch for real; the test VM's GRUB can't install, and a real guest boots systemd-boot.
      boot.loader.grub.enable = false;
      environment.etc."pocketnix-test/id_ed25519" = {
        source = snakeOilEd25519PrivateKey;
        mode = "0600";
      };
      programs.ssh.knownHosts.builder.publicKey = snakeOilEd25519PublicKey;
      nix.settings = {
        substituters = pkgs.lib.mkForce ["http://builder:5000"];
        trusted-public-keys = [cachePublic];
      };
      # Two later systems for the update subtests, apart only in a marker file.
      system.build.next =
        pkgs.lib.genAttrs ["1" "2"] (n:
          (extendModules {modules = [{environment.etc."pocketnix-test-generation".text = n;}];}).config.system.build.toplevel);
      networking.firewall.allowedTCPPorts = [9100];
      users.users.nixos.openssh.authorizedKeys.keys = [snakeOilPublicKey];
      specialisation.retheme.configuration.pocketnix.guest.terminal.theme.colors.background = pkgs.lib.mkForce "#123456";
    };

    # A build host (over ssh, for next.1) and a binary cache plus published
    # path (for next.2) in one machine.
    nodes.builder = {nodes, ...}: let
      inherit (nodes.phone.system.build) next;
    in {
      # A fixed host key, which the phone lists in its known hosts.
      services.openssh = {
        enable = true;
        hostKeys = [
          {
            path = "/etc/ssh/ssh_host_ed25519_key";
            type = "ed25519";
          }
        ];
      };
      environment.etc."ssh/ssh_host_ed25519_key" = {
        source = snakeOilEd25519PrivateKey;
        mode = "0600";
      };
      users.users.nix = {
        isNormalUser = true;
        openssh.authorizedKeys.keys = [snakeOilEd25519PublicKey];
      };
      environment.etc."pocketnix-next".source = next."1";
      virtualisation.additionalPaths = [next."1" next."2"];
      services.nix-serve = {
        enable = true;
        secretKeyFile = "${cacheSecret}";
      };
      services.nginx = {
        enable = true;
        virtualHosts.default.locations."= /toplevel".return = "200 '${next."2"}'";
      };
      networking.firewall.allowedTCPPorts = [80 5000];
      virtualisation.memorySize = 1536;
    };

    nodes.client = {
      environment.systemPackages = [cli];
    };

    # By IP: the CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch order.
    testScript = {nodes, ...}: let
      inherit (nodes.phone.system.build) next;
    in ''
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
          assert cfg["forwards"] == [{"name": "ssh", "guestPort": 22, "hostPort": ${toString nodes.phone.pocketnix.guest.forwards.ssh.hostPort}, "lan": False}], cfg["forwards"]
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

      with subtest("the command palette gets the module's snippets and the user's history"):
          snippets = {s["name"]: s for s in cfg["terminal"]["snippets"]}
          assert snippets["Disk usage"] == {"name": "Disk usage", "command": "df -h /", "submit": True}, snippets
          phone.succeed("printf 'ls\\n#1700000000\\nuname -a\\nls\\n' > /home/nixos/.bash_history")
          out = client.succeed(f"{A} history").splitlines()
          assert out == ["ls", "uname -a"], out

      with subtest("an update from the build host arrives over ssh and switches"):
          # Test VMs boot without a system profile; a real guest has one from its first boot.
          phone.succeed("nix-env -p /nix/var/nix/profiles/system --set $(readlink -f /run/current-system)")
          builder.wait_for_unit("sshd.service")
          builder.succeed("systemctl stop nix-serve.service")
          u = json.loads(client.succeed(f"{A} update-check"))
          assert u["available"] == "${next."1"}" and u["source"] == "buildHost", u
          u = json.loads(client.succeed(f"{A} update"))
          assert u["current"] == "${next."1"}" and "available" not in u, u
          assert any("copying from nix@builder" in l for l in u["log"]), u["log"]
          phone.succeed("grep -qx 1 /etc/pocketnix-test-generation")
          assert phone.succeed("systemctl show -P MainPID pocketnix-agent") == pid

      with subtest("with the build host down, the published path is substituted"):
          builder.succeed("systemctl start nix-serve.service")
          builder.succeed("systemctl stop sshd.service")
          builder.wait_for_open_port(5000)
          builder.wait_for_open_port(80)
          u = json.loads(client.succeed(f"{A} update"))
          assert u["current"] == "${next."2"}", u
          phone.succeed("grep -qx 2 /etc/pocketnix-test-generation")

      with subtest("a rollback switches to an earlier generation"):
          u = json.loads(client.succeed(f"{A} updates"))
          gens = {g["number"]: g for g in u["generations"]}
          assert sorted(gens) == [1, 2, 3] and gens[3]["current"], u["generations"]
          u = json.loads(client.succeed(f"{A} switch-generation 2"))
          assert u["current"] == "${next."1"}", u
          phone.succeed("grep -qx 1 /etc/pocketnix-test-generation")

      with subtest("poweroff over the control stream shuts the guest down"):
          client.succeed(f"{A} poweroff")
          phone.wait_for_shutdown()
    '';
  }
