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
  # Calls the FileChooser backend as xdg-desktop-portal would, as the user.
  portalCall = pkgs.writeShellScript "portal-call" ''
    exec ${pkgs.util-linux}/bin/runuser -u nixos -- ${pkgs.coreutils}/bin/env XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus \
      ${pkgs.systemd}/bin/busctl --user --json=short --timeout=120 call \
      org.freedesktop.impl.portal.desktop.pocketnix /org/freedesktop/portal/desktop org.freedesktop.impl.portal.FileChooser "$@"
  '';
  # Runs a guest command as the user in the background, its stdout in /tmp/<unit>.out and status in /tmp/<unit>.status.
  guestBg = pkgs.writeShellScript "guest-bg" ''
    unit=$1
    shift
    exec ${pkgs.systemd}/bin/systemd-run --unit="$unit" -p User=nixos -E PATH=/run/current-system/sw/bin \
      ${pkgs.bash}/bin/sh -c '"$@" > /tmp/'"$unit"'.out; echo $? > /tmp/'"$unit"'.status' sh "$@"
  '';
  # The published path is https only: a self-signed certificate for the builder's address, trusted by the phone.
  tlsFor = ip:
    pkgs.runCommand "pocketnix-test-tls" {nativeBuildInputs = [pkgs.openssl];} ''
      mkdir $out
      openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
        -subj /CN=builder -addext "subjectAltName=IP:${ip},DNS:builder" -keyout $out/key -out $out/cert
    '';
in
  pkgs.testers.runNixOSTest {
    name = "pocketnix-agent";

    nodes.phone = {
      config,
      nodes,
      extendModules,
      ...
    }: {
      imports = [self.nixosModules.default ../example/configuration.nix];
      pocketnix.guest = {
        avf.enable = false;
        agent.listen = ["vsock:9100" "tcp:0.0.0.0:9100"];
        # On ssh's host port: TCP and UDP forwards are separate spaces.
        forwards.echo = {
          guestPort = 7777;
          hostPort = config.pocketnix.guest.forwards.ssh.hostPort;
          protocol = "udp";
        };
        terminal.profiles = {
          # The size comes first: the CLI resizes on the session's first output.
          probe.command = ''stty size; id -un; read line; stty size; echo "got:$line"'';
          fail.command = "exit 3";
          # Blocks until the action comes back; the bus address is the agent's doing.
          notify.command = ''printf 'bus:%s\n' "$DBUS_SESSION_BUS_ADDRESS"; notify-send -A ok=OK -w build waits; echo "exit:$?"'';
        };
        forwardOffers.enable = true;
        terminal.snippets.probe = {
          command = "echo snippet";
          submit = true;
        };
        updates = {
          buildHost = {
            host = "pocketnix-build@builder";
            # A symlink to a store path is an installable too.
            installable = "/etc/pocketnix-next";
            sshKey = "/etc/pocketnix-test/id_ed25519";
          };
          # By IP: the agent's pure-Go resolver.
          published.url = "https://${nodes.builder.networking.primaryIPAddress}/toplevel";
          checkInterval = null;
        };
      };
      systemd.services.udp-echo = {
        wantedBy = ["multi-user.target"];
        # A colon would end socat's address, so the reply lives in a script; one
        # write, as socat sends each read as its own datagram.
        serviceConfig.ExecStart = "${pkgs.socat}/bin/socat UDP4-RECVFROM:7777,bind=127.0.0.1,fork EXEC:${pkgs.writeShellScript "udp-echo" ''l=$(cat); printf 'echo:%s' "$l"''}";
      };
      # Updates switch for real; the test VM's GRUB can't install, and a real guest boots systemd-boot.
      boot.loader.grub.enable = false;
      environment.etc."pocketnix-test/id_ed25519" = {
        source = snakeOilEd25519PrivateKey;
        mode = "0600";
      };
      programs.ssh.knownHosts.builder.publicKey = snakeOilEd25519PublicKey;
      security.pki.certificateFiles = ["${tlsFor nodes.builder.networking.primaryIPAddress}/cert"];
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
      # The FileChooser portal is a user service on the session bus.
      xdg.portal.enable = true;
      users.users.nixos.linger = true;
      specialisation.retheme.configuration.pocketnix.guest.terminal.theme.colors.background = pkgs.lib.mkForce "#123456";
      # The port watcher's /proc poller, which this kernel's eBPF otherwise leaves unused.
      specialisation.poller.configuration.systemd.services.pocketnix-agent.serviceConfig.ExecStart =
        pkgs.lib.mkForce "${config.systemd.services.pocketnix-agent.serviceConfig.ExecStart} --poll-ports";
    };

    # A build host (over ssh, for next.1) and a binary cache plus published
    # path (for next.2) in one machine.
    nodes.builder = {nodes, ...}: let
      inherit (nodes.phone.system.build) next;
    in {
      imports = [self.nixosModules.buildHost];
      # The phone's key only builds /etc/pocketnix-next and serves the copy.
      pocketnix.buildHost = {
        enable = true;
        guests.phone = {
          publicKey = snakeOilEd25519PublicKey;
          installable = "/etc/pocketnix-next";
        };
      };
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
      environment.etc."pocketnix-next".source = next."1";
      virtualisation.additionalPaths = [next."1" next."2"];
      services.nix-serve = {
        enable = true;
        secretKeyFile = "${cacheSecret}";
      };
      services.nginx = {
        enable = true;
        virtualHosts.default = let
          tls = tlsFor nodes.builder.networking.primaryIPAddress;
        in {
          onlySSL = true;
          sslCertificate = "${tls}/cert";
          sslCertificateKey = "${tls}/key";
          locations."= /toplevel".return = "200 '${next."2"}'";
        };
      };
      networking.firewall.allowedTCPPorts = [443 5000];
      virtualisation.memorySize = 1536;
    };

    nodes.client = {
      environment.systemPackages = [cli];
    };

    # By IP: the CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch order.
    testScript = {nodes, ...}: let
      inherit (nodes.phone.system.build) next;
      inherit (nodes.phone.pocketnix.guest) terminal user;
      lanPython =
        if nodes.phone.pocketnix.guest.forwardOffers.lan
        then "True"
        else "False";
    in ''
      import json
      import shlex
      import time
      import urllib.parse

      # Absolute: systemd-run units don't get the system PATH.
      A = "${cli}/bin/pocketnix agent --connect tcp:${nodes.phone.networking.primaryIPAddress}:9100"

      start_all()
      phone.wait_for_unit("pocketnix-agent.service")
      phone.wait_for_open_port(9100)
      client.wait_for_unit("multi-user.target")

      with subtest("the control stream delivers the module's config"):
          cfg = json.loads(client.succeed(f"{A} config"))
          assert cfg["hostname"] == "${nodes.phone.networking.hostName}", cfg["hostname"]
          ssh_port = ${toString nodes.phone.pocketnix.guest.forwards.ssh.hostPort}
          forwards = {f["name"]: f for f in cfg["forwards"]}
          fields = ("guestPort", "hostPort", "protocol")
          assert {k: forwards["echo"][k] for k in fields} == {"guestPort": 7777, "hostPort": ssh_port, "protocol": "udp"}, forwards
          assert {k: forwards["ssh"][k] for k in fields} == {"guestPort": 22, "hostPort": ssh_port, "protocol": "tcp"}, forwards
          names = {p["name"] for p in cfg["terminal"]["profiles"]}
          assert {"fail", "notify", "probe", "${terminal.defaultProfile}"} <= names, names
          assert cfg["terminal"]["defaultProfile"] == "${terminal.defaultProfile}"
          assert cfg["terminal"]["theme"]["colors"]["background"] == "${terminal.theme.colors.background}"
          assert cfg["terminal"]["extraKeys"][0][0] == {"key": "${(builtins.head (builtins.head terminal.extraKeys)).key}"}, cfg["terminal"]["extraKeys"]
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
          phone.wait_for_open_port(22)
          client.succeed("install -m 600 ${snakeOilPrivateKey} /tmp/key")
          out = client.succeed(
              "ssh -i /tmp/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "
              f"-o ProxyCommand='{A} forward 22' nixos@phone echo via-forward"
          )
          assert "via-forward" in out, out

      with subtest("a udp stream relays datagrams to a guest UDP port"):
          phone.wait_until_succeeds("ss -uln | grep -q 127.0.0.1:7777")
          # Each datagram is its own socat child, so replies may cross, and on a loaded machine come late.
          client.succeed(f"systemd-run --unit=udp sh -c \"printf 'one\\ntwo\\n' | {A} udp --wait 120s 7777 > /tmp/udp\"")
          client.wait_until_succeeds("test $(grep -c '^echo:' /tmp/udp) -ge 2", timeout=120)
          out = client.succeed("cat /tmp/udp")
          assert sorted(out.split()) == ["echo:one", "echo:two"], out

      with subtest("a switch reloads the config live, without restarting the agent"):
          pid = phone.succeed("systemctl show -P MainPID pocketnix-agent")
          client.succeed(f"systemd-run --unit=watch sh -c '{A} watch > /tmp/watch'")
          client.wait_until_succeeds("grep -q '${terminal.theme.colors.background}' /tmp/watch", timeout=60)
          phone.succeed("/run/current-system/specialisation/retheme/bin/switch-to-configuration test")
          client.wait_until_succeeds("tail -n1 /tmp/watch | grep -q '#123456'", timeout=60)
          assert phone.succeed("systemctl show -P MainPID pocketnix-agent") == pid

      with subtest("the command palette gets the module's snippets and the user's history"):
          snippets = {s["name"]: s for s in cfg["terminal"]["snippets"]}
          assert snippets["probe"] == {"name": "probe", "command": "echo snippet", "submit": True}, snippets
          phone.succeed("printf 'ls\\n#1700000000\\nuname -a\\nls\\n' > /home/nixos/.bash_history")
          out = client.succeed(f"{A} history").splitlines()
          assert out == ["ls", "uname -a"], out

      with subtest("guest programs notify, open and copy through the app"):
          client.succeed(f"systemd-run --unit=events sh -c '{A} events > /tmp/events'")
          # Refused until the events stream is connected.
          phone.wait_until_succeeds("runuser -u nixos -- pocketnix-guest notify --title build 'all done'", timeout=30)
          client.wait_until_succeeds("grep '\"title\":\"build\"' /tmp/events | grep '\"body\":\"all done\"' >/dev/null", timeout=30)
          phone.succeed("runuser -u nixos -- xdg-open https://nixos.org")
          client.wait_until_succeeds("grep -q '\"url\":\"https://nixos.org\"' /tmp/events", timeout=30)
          phone.succeed("runuser -u nixos -- sh -c 'echo -n copied | pocketnix-guest copy'")
          client.wait_until_succeeds("grep '\"type\":\"guest-clipboard\"' /tmp/events | grep '\"text\":\"copied\"' >/dev/null", timeout=30)
          client.succeed(f"{A} set-clipboard from-android")
          assert phone.succeed("runuser -u nixos -- pocketnix-guest paste") == "from-android"
          # Only root and the session user may use the guest socket.
          phone.fail("runuser -u nobody -- ${nodes.phone.pocketnix.guest.package}/bin/pocketnix-guest paste")

      # The port-listeners snapshots an events stream's output holds, oldest first.
      def snapshots(events: str) -> list[dict]:
          pushes = [json.loads(l) for l in client.succeed(f"cat {events}").splitlines()]
          return [p for p in pushes if p["type"] == "port-listeners"]

      def await_ports(events: str, ok) -> None:
          retry(lambda _: bool(snapshots(events)) and ok(snapshots(events)[-1]["ports"]), timeout_seconds=60)

      # Root's listener is up before the user's, so the scan that finds the user's sees it too.
      def listeners_reach_the_app(events: str) -> None:
          phone.succeed("systemd-run --unit=root-listener ${pkgs.socat}/bin/socat TCP-LISTEN:8124,fork EXEC:${pkgs.coreutils}/bin/cat")
          phone.wait_for_open_port(8124)
          phone.succeed("systemd-run --unit=user-listener -p User=${user} ${pkgs.socat}/bin/socat TCP-LISTEN:8123,fork EXEC:${pkgs.coreutils}/bin/cat")
          await_ports(events, lambda ports: 8123 in ports)
          assert all(8124 not in p["ports"] for p in snapshots(events)), snapshots(events)
          assert snapshots(events)[-1]["lan"] == ${lanPython}, snapshots(events)[-1]
          phone.succeed("systemctl stop user-listener")
          await_ports(events, lambda ports: ports == [])
          phone.succeed("systemctl stop root-listener")

      with subtest("a user's listener reaches the app, and root's never does"):
          listeners_reach_the_app("/tmp/events")

      with subtest("the agent watches listeners with eBPF"):
          print(phone.succeed("journalctl -u pocketnix-agent -o cat | grep 'port watcher: eBPF'"))

      with subtest("a forward reaches a listener on ::1 alone"):
          phone.succeed("systemd-run --unit=v6-listener -p User=${user} ${pkgs.socat}/bin/socat TCP6-LISTEN:8125,bind=[::1],fork EXEC:${pkgs.coreutils}/bin/cat")
          await_ports("/tmp/events", lambda ports: 8125 in ports)
          assert client.succeed(f"printf v6 | timeout 60 {A} forward 8125") == "v6"
          phone.succeed("systemctl stop v6-listener")

      with subtest("a port root also listens on is neither offered nor forwarded"):
          # A forward dials 127.0.0.1 first: root's would answer for the user's ::1.
          phone.succeed("systemd-run --unit=root-v4 ${pkgs.socat}/bin/socat TCP4-LISTEN:8126,bind=127.0.0.1,fork EXEC:${pkgs.coreutils}/bin/cat")
          phone.succeed("systemd-run --unit=user-v6 -p User=${user} ${pkgs.socat}/bin/socat TCP6-LISTEN:8126,bind=[::1],fork EXEC:${pkgs.coreutils}/bin/cat")
          phone.wait_until_succeeds("test $(ss -tlnH 'sport = :8126' | wc -l) = 2", timeout=60)
          # Started after both, so the scan that finds it sees them too.
          phone.succeed("systemd-run --unit=user-later -p User=${user} ${pkgs.socat}/bin/socat TCP-LISTEN:8127,fork EXEC:${pkgs.coreutils}/bin/cat")
          await_ports("/tmp/events", lambda ports: 8127 in ports)
          assert all(8126 not in p["ports"] for p in snapshots("/tmp/events")), snapshots("/tmp/events")
          client.fail(f"printf x | timeout 60 {A} forward 8126")
          phone.succeed("systemctl stop root-v4 user-v6 user-later")

      with subtest("the session bus: notify-send's actions come back, gio opens on Android"):
          # Lingering, with no login, starts the user's bus.
          phone.wait_for_unit("dbus.socket", "nixos")
          phone.succeed("test -S /run/user/1000/bus")
          client.succeed(f"systemd-run --unit=notify-tab sh -c '{A} exec --profile notify > /tmp/notify-tab'")
          client.wait_until_succeeds("grep -q '\"body\":\"waits\"' /tmp/events", timeout=60)
          ev = [json.loads(l) for l in client.succeed("cat /tmp/events").splitlines() if '"waits"' in l][-1]
          assert ev["actions"] == [{"key": "ok", "label": "OK"}] and ev["appName"] == "notify-send", ev
          client.succeed(f"{A} notify-action {ev['notification']} ok")
          client.wait_until_succeeds("grep -q exit: /tmp/notify-tab", timeout=30)
          out = client.succeed("cat /tmp/notify-tab")
          assert "bus:unix:path=/run/user/1000/bus" in out and "\nok\r" in out and "exit:0" in out, out
          # The desktop defaults lead to Android, not only xdg-open.
          phone.succeed("runuser -u nixos -- env XDG_RUNTIME_DIR=/run/user/1000 ${pkgs.glib.bin}/bin/gio open https://nixos.org/learn")
          client.wait_until_succeeds("grep -q '\"url\":\"https://nixos.org/learn\"' /tmp/events", timeout=30)

      with subtest("shares and the file provider reach the user's files"):
          client.succeed("echo picture > /tmp/pic.jpg")
          out = client.succeed(f"{A} share /tmp/pic.jpg").strip()
          assert out == "/home/nixos/Inbox/pic.jpg", out
          phone.succeed("test \"$(stat -c %U /home/nixos/Inbox/pic.jpg)\" = nixos")
          client.succeed(f"echo doc | {A} put notes.txt")
          phone.succeed("grep -qx doc /home/nixos/notes.txt")
          phone.succeed("test \"$(stat -c %U /home/nixos/notes.txt)\" = nixos")
          assert client.succeed(f"{A} cat Inbox/pic.jpg") == "picture\n"
          names = [json.loads(l)["name"] for l in client.succeed(f"{A} ls").splitlines()]
          assert "notes.txt" in names and "Inbox" in names, names
          client.fail(f"{A} cat ../../etc/shadow")
          # A ranged edit lands in place, keeps the owner, and leaves no temp or record.
          client.succeed(f"printf XY | {A} patch notes.txt 1")
          phone.succeed("test \"$(cat /home/nixos/notes.txt)\" = dXY")
          phone.succeed("test \"$(stat -c %U /home/nixos/notes.txt)\" = nixos")
          phone.succeed("test -z \"$(ls -A /var/lib/pocketnix/file-temps)\"")
          phone.fail("ls -A /home/nixos | grep -q pocketnix-")

      # Transfers: the CLI's `events` (still running) shows each request, and the CLI answers it as the app would.
      seen = [len(client.succeed("cat /tmp/events").splitlines())]

      def await_event(kind):
          for _ in range(120):
              lines = client.succeed("cat /tmp/events").splitlines()
              for i in range(seen[0], len(lines)):
                  e = json.loads(lines[i])
                  if e["type"] == kind:
                      seen[0] = i + 1
                      return e
              time.sleep(0.5)
          raise Exception(f"no {kind} event")

      def guest_bg(unit, *cmd):
          phone.succeed("${guestBg} " + unit + " " + " ".join(shlex.quote(c) for c in cmd))

      def guest_status(unit):
          phone.wait_until_succeeds(f"test -s /tmp/{unit}.status", timeout=60)
          return int(phone.succeed(f"cat /tmp/{unit}.status"))

      with subtest("guest programs share files and text through the app"):
          phone.succeed("runuser -u nixos -- pocketnix-guest share /home/nixos/notes.txt")
          assert await_event("share")["paths"] == ["notes.txt"]
          phone.fail("runuser -u nixos -- pocketnix-guest share /etc/hostname")
          phone.succeed("runuser -u nixos -- pocketnix-guest share --text hello there")
          assert await_event("share")["text"] == "hello there"

      with subtest("a pick lands the app's files where the guest asked"):
          phone.succeed("runuser -u nixos -- mkdir /home/nixos/picked")
          guest_bg("pick", "pocketnix-guest", "pick", "--multiple", "--type", "image/*", "--dest", "/home/nixos/picked")
          e = await_event("pick")
          assert e["multiple"] and e["types"] == ["image/*"], e
          client.succeed("echo second > /tmp/two.jpg")
          client.succeed(f"{A} pick-answer {e['ref']} /tmp/pic.jpg /tmp/two.jpg")
          assert guest_status("pick") == 0
          assert phone.succeed("cat /tmp/pick.out").split() == ["/home/nixos/picked/pic.jpg", "/home/nixos/picked/two.jpg"]
          phone.succeed("grep -qx picture /home/nixos/picked/pic.jpg")
          phone.succeed("test \"$(stat -c %U /home/nixos/picked/two.jpg)\" = nixos")
          guest_bg("pick2", "pocketnix-guest", "pick")
          client.succeed(f"{A} pick-answer {await_event('pick')['ref']}")
          assert guest_status("pick2") == 1

      with subtest("a save sends a file the user can read, from anywhere"):
          phone.succeed("runuser -u nixos -- sh -c 'echo report > /tmp/report.txt'")
          guest_bg("save", "pocketnix-guest", "save", "--name", "r.txt", "/tmp/report.txt")
          e = await_event("save")
          assert e["name"] == "r.txt" and e["size"] == 7, e
          client.succeed(f"{A} save-answer {e['ref']} /tmp/saved.txt")
          assert client.succeed("cat /tmp/saved.txt") == "report\n"
          assert guest_status("save") == 0
          # The user opens the file, so the root agent never reads one for them.
          phone.fail("runuser -u nixos -- pocketnix-guest save /etc/shadow")

      with subtest("typed clipboard data crosses only when asked for"):
          phone.succeed("printf 'PNG-data' > /tmp/img.png")
          guest_bg("copy", "sh", "-c", "pocketnix-guest copy --type image/png < /tmp/img.png")
          e = await_event("guest-clipboard-data")
          assert e["mime"] == "image/png" and e["size"] == 8, e
          client.succeed(f"{A} save-answer {e['ref']} /tmp/clip.png")
          assert client.succeed("cat /tmp/clip.png") == "PNG-data"
          assert guest_status("copy") == 0
          assert phone.succeed("runuser -u nixos -- pocketnix-guest paste --list-types") == "image/png\n"
          client.succeed(f"{A} set-clipboard --type image/jpeg --type text/plain caption")
          assert phone.succeed("runuser -u nixos -- pocketnix-guest paste --list-types").split() == ["image/jpeg", "text/plain"]
          assert phone.succeed("runuser -u nixos -- pocketnix-guest paste") == "caption"
          client.succeed("printf jpeg-bytes > /tmp/a.jpg")
          guest_bg("paste", "pocketnix-guest", "paste", "--type", "image/jpeg")
          e = await_event("clipboard-request")
          assert e["mime"] == "image/jpeg", e
          client.succeed(f"{A} paste-answer {e['ref']} /tmp/a.jpg")
          assert guest_status("paste") == 0
          assert phone.succeed("cat /tmp/paste.out") == "jpeg-bytes"

      with subtest("the FileChooser portal backend picks and saves through the app"):
          phone.wait_for_unit("user@1000.service")
          phone.succeed("systemd-run --unit=portal-open sh -c '${portalCall} OpenFile osssa{sv} /org/freedesktop/portal/desktop/request/1_1/a test \"\" Open 1 multiple b true > /tmp/open.json'")
          e = await_event("pick")
          assert e["multiple"], e
          client.succeed("echo doc > /tmp/doc.pdf")
          client.succeed(f"{A} pick-answer {e['ref']} /tmp/doc.pdf")
          phone.wait_until_succeeds("grep -q uris /tmp/open.json", timeout=60)
          out = json.loads(phone.succeed("cat /tmp/open.json"))
          assert out["data"][0] == 0 and out["data"][1]["uris"]["data"] == ["file:///home/nixos/Inbox/doc.pdf"], out
          phone.succeed("systemd-run --unit=portal-cancel sh -c '${portalCall} OpenFile osssa{sv} /org/freedesktop/portal/desktop/request/1_1/b test \"\" Open 0 > /tmp/cancel.json'")
          client.succeed(f"{A} pick-answer {await_event('pick')['ref']}")
          phone.wait_until_succeeds("test -s /tmp/cancel.json", timeout=60)
          assert json.loads(phone.succeed("cat /tmp/cancel.json"))["data"][0] == 1
          # Android's picker comes first; the app writes the file the portal named, which then goes there.
          phone.succeed("systemd-run --unit=portal-save sh -c '${portalCall} SaveFile osssa{sv} /org/freedesktop/portal/desktop/request/1_1/c test \"\" Save 1 current_name s out.txt > /tmp/save.json'")
          e = await_event("save")
          assert e["name"] == "out.txt" and "size" not in e, e
          client.succeed(f"systemd-run --unit=save-answer {A} save-answer {e['ref']} /tmp/portal-saved.txt")
          phone.wait_until_succeeds("grep -q uris /tmp/save.json", timeout=60)
          out = json.loads(phone.succeed("cat /tmp/save.json"))
          path = urllib.parse.unquote(out["data"][1]["uris"]["data"][0].removeprefix("file://"))
          assert path.startswith("/home/nixos/.cache/pocketnix/saves/") and path.endswith("/out.txt"), path
          phone.succeed(f"runuser -u nixos -- sh -c 'echo saved-by-app > {path}'")
          client.wait_until_succeeds("grep -qx saved-by-app /tmp/portal-saved.txt", timeout=60)
          phone.wait_until_succeeds(f"! test -e {path}", timeout=30)

      with subtest("a sleep inhibitor in a tab asks the phone to stay awake"):
          client.succeed(f"systemd-run --unit=wake sh -c '{A} wake > /tmp/wake'")
          client.wait_until_succeeds("grep -q '\"held\":false' /tmp/wake", timeout=30)
          # A tab has no logind session: the module's polkit rule lets the user inhibit anyway.
          client.succeed(f"systemd-run --unit=inhibit {A} exec --input $'systemd-inhibit --what=sleep --who=rsync --why=backup sleep 5; exit\\r'")
          client.wait_until_succeeds("grep '\"held\":true' /tmp/wake | grep '\"who\":\"rsync\"' | grep '\"why\":\"backup\"' >/dev/null", timeout=30)
          client.wait_until_succeeds("tail -n1 /tmp/wake | grep -q '\"held\":false'", timeout=30)

      with subtest("the phone's state drives targets, host.json and the timezone"):
          def active(unit):
              return phone.execute(f"systemctl is-active {unit}")[0] == 0
          client.succeed(f"""systemd-run --unit=host1 {A} host-state '{{"timeZone":"Europe/Copenhagen","online":true,"charging":true}}'""")
          phone.wait_until_succeeds("systemctl is-active pocketnix-online.target pocketnix-unmetered.target pocketnix-charging.target", timeout=30)
          assert not active("pocketnix-power-save.target")
          phone.wait_until_succeeds("timedatectl show -p Timezone --value | grep -qx Europe/Copenhagen", timeout=30)
          st = json.loads(phone.succeed("runuser -u nixos -- pocketnix-guest host"))
          assert st["online"] and st["charging"] and st["connected"] and st["timeZone"] == "Europe/Copenhagen", st
          assert json.loads(phone.succeed("cat /run/pocketnix/host.json")) == st
          # A newer stream's state replaces it; a metered network isn't unmetered.
          client.succeed(f"""systemd-run --unit=host2 {A} host-state '{{"timeZone":"America/New_York","online":true,"metered":true,"powerSave":true}}'""")
          phone.wait_until_succeeds("systemctl is-active pocketnix-power-save.target", timeout=30)
          assert active("pocketnix-online.target") and not active("pocketnix-unmetered.target") and not active("pocketnix-charging.target")
          phone.wait_until_succeeds("timedatectl show -p Timezone --value | grep -qx America/New_York", timeout=30)
          client.succeed("systemctl stop host1")
          # Without the app the phone is offline as far as the guest knows; the zone stays.
          client.succeed("systemctl stop host2")
          phone.wait_until_fails("systemctl is-active pocketnix-online.target", timeout=30)
          assert active("pocketnix-power-save.target")
          assert not json.loads(phone.succeed("cat /run/pocketnix/host.json"))["connected"]
          phone.succeed("timedatectl show -p Timezone --value | grep -qx America/New_York")

      with subtest("an Android folder mounts in the user's home while the app serves it"):
          M = "/home/nixos/Android/Pictures"
          U = "runuser -u nixos --"
          client.succeed("mkdir -p /tmp/Pictures/sub && echo hello > /tmp/Pictures/a.txt")
          client.succeed(f"systemd-run --unit=hostfs {A} hostfs-serve /tmp/Pictures")
          phone.wait_until_succeeds(f"{U} grep -qx hello {M}/a.txt", timeout=30)
          assert phone.succeed(f"{U} ls {M}").split() == ["a.txt", "sub"]
          assert phone.succeed(f"{U} stat -c %U {M}/a.txt").strip() == "nixos"
          # Not even root gets in: no allow_other.
          phone.fail(f"ls {M}")
          phone.succeed(f"{U} sh -c 'echo written > {M}/sub/new.txt'")
          client.succeed("grep -qx written /tmp/Pictures/sub/new.txt")
          phone.succeed(f"{U} truncate -s 3 {M}/sub/new.txt")
          client.succeed("test \"$(cat /tmp/Pictures/sub/new.txt)\" = wri")
          phone.succeed(f"{U} sh -c 'echo again >> {M}/sub/new.txt'")
          client.succeed("test \"$(cat /tmp/Pictures/sub/new.txt)\" = wriagain")
          phone.succeed(f"{U} mv {M}/sub/new.txt {M}/moved.txt")
          client.succeed("test -f /tmp/Pictures/moved.txt && ! test -e /tmp/Pictures/sub/new.txt")
          # Saving through a temp file renames over the original.
          phone.succeed(f"{U} sh -c 'echo saved > {M}/.a.swp && mv {M}/.a.swp {M}/a.txt'")
          client.succeed("grep -qx saved /tmp/Pictures/a.txt")
          phone.fail(f"{U} rmdir {M}")
          phone.succeed(f"{U} rm {M}/moved.txt && {U} rmdir {M}/sub")
          client.succeed("! test -e /tmp/Pictures/moved.txt && ! test -e /tmp/Pictures/sub")

      with subtest("with the app gone a host folder fails fast and stays mounted, until the app is back"):
          client.succeed("systemctl stop hostfs")
          # Until the agent sees the streams close.
          phone.wait_until_fails(f"{U} cat {M}/a.txt", timeout=10)
          out = phone.fail(f"{U} cat {M}/a.txt 2>&1")
          assert "Input/output error" in out, out
          phone.succeed(f"findmnt -n {M}")
          phone.succeed(f"{U} sh -c 'cd {M} && pwd'")
          client.succeed(f"systemd-run --unit=hostfs-again {A} hostfs-serve /tmp/Pictures")
          phone.wait_until_succeeds(f"{U} grep -qx saved {M}/a.txt", timeout=30)

      with subtest("a folder the app no longer shares is unmounted, and a read-only one refuses writes"):
          client.succeed("mkdir -p /tmp/Docs && echo doc > /tmp/Docs/d.txt")
          client.succeed("systemctl stop hostfs-again")
          client.succeed(f"systemd-run --unit=hostfs-docs {A} hostfs-serve --read-only /tmp/Docs")
          phone.wait_until_succeeds(f"{U} grep -qx doc /home/nixos/Android/Docs/d.txt", timeout=30)
          phone.fail(f"findmnt -n {M}")
          phone.fail(f"test -e {M}")
          out = phone.fail(f"{U} sh -c 'echo x > /home/nixos/Android/Docs/d.txt' 2>&1")
          assert "Read-only file system" in out, out
          client.succeed("systemctl stop hostfs-docs")

      with subtest("an update from the build host arrives over ssh and switches"):
          # Test VMs boot without a system profile; a real guest has one from its first boot.
          phone.succeed("nix-env -p /nix/var/nix/profiles/system --set $(readlink -f /run/current-system)")
          builder.wait_for_unit("sshd.service")
          builder.succeed("systemctl stop nix-serve.service")
          u = json.loads(client.succeed(f"{A} update-check"))
          assert u["available"] == "${next."1"}" and u["source"] == "buildHost", u
          u = json.loads(client.succeed(f"{A} update"))
          assert u["current"] == "${next."1"}" and "available" not in u, u
          assert any("copying from pocketnix-build@builder" in l for l in u["log"]), u["log"]
          client.succeed("grep -qF '\"who\":\"pocketnix\"' /tmp/wake")
          phone.succeed("grep -qx 1 /etc/pocketnix-test-generation")
          assert phone.succeed("systemctl show -P MainPID pocketnix-agent") == pid

      with subtest("the build host's key runs nothing but the build and the copy"):
          ssh = "timeout 120 ssh -o BatchMode=yes -i /etc/pocketnix-test/id_ed25519 pocketnix-build@builder"
          out = phone.fail(f"{ssh} id </dev/null 2>&1")
          assert "refusing: id" in out, out
          phone.fail(f"{ssh} -W localhost:5000 </dev/null")
          # Whatever installable it asks for, it gets the pinned one.
          out = phone.succeed(f"{ssh} nix build --print-out-paths /nix/store/nonexistent </dev/null 2>&1")
          assert out.strip().splitlines()[-1] == "${next."1"}", out

      with subtest("with the build host down, the published path is substituted"):
          builder.succeed("systemctl start nix-serve.service")
          builder.succeed("systemctl stop sshd.service")
          builder.wait_for_open_port(5000)
          builder.wait_for_open_port(443)
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

      with subtest("with the /proc poller, a user's listener reaches the app, and root's never does"):
          phone.succeed("${nodes.phone.system.build.toplevel}/specialisation/poller/bin/switch-to-configuration test")
          # A switch only reloads the agent.
          phone.succeed("systemctl restart pocketnix-agent")
          phone.wait_for_open_port(9100)
          phone.wait_until_succeeds("journalctl -u pocketnix-agent -o cat | grep 'port watcher: polling /proc'", timeout=60)
          client.succeed(f"systemd-run --unit=events-poller sh -c '{A} events > /tmp/events-poller'")
          listeners_reach_the_app("/tmp/events-poller")

      with subtest("poweroff over the control stream shuts the guest down"):
          client.succeed(f"{A} poweroff")
          phone.wait_for_shutdown()
    '';
  }
