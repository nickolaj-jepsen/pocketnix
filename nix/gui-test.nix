# A guest with gui.enable: socket activation brings up the compositor for a
# Wayland client, and the agent's gui stream reaches it and gets its replay,
# also after the agent restarts.
{
  pkgs,
  self,
}: let
  cli = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli;
  # Speaks the app's side of docs/gui-protocol.md through the command it's
  # given (`pocketnix agent … gui`) up to `ready`, and prints what the replay
  # held as JSON. Fails without a toplevel of --want-app.
  probe = pkgs.writers.writePython3Bin "gui-probe" {flakeIgnore = ["E501"];} ''
    import json
    import struct
    import subprocess
    import sys

    JSON, COMMIT, COMMIT_DATA = 0x00, 0x01, 0x02
    HEADER = ">HHIIIHBBBBHiiIIIIIIIIiIIIQ"


    def read_exact(f, n):
        b = f.read(n)
        if len(b) != n:
            sys.exit(f"stream ended after {len(b)} of {n} bytes")
        return b


    def read_frame(f):
        typ, length = struct.unpack(">BI", read_exact(f, 5))
        if length > 1 << 20:
            sys.exit(f"frame of {length} bytes")
        return typ, read_exact(f, length)


    def send_json(f, msg):
        b = json.dumps(msg).encode()
        f.write(struct.pack(">BI", JSON, len(b)) + b)
        f.flush()


    args = sys.argv[1:]
    want = None
    if args[:1] == ["--want-app"]:
        want, args = args[1], args[2:]
    p = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    send_json(p.stdin, {"type": "hello", "version": 1, "minVersion": 1, "app": "gui-probe", "hashes": False})
    out = {"toplevels": [], "surfaces": [], "commits": []}
    pending = None
    while True:
        typ, payload = read_frame(p.stdout)
        if pending is not None and typ != COMMIT_DATA:
            sys.exit(f"frame {typ} inside commit {pending['seq']}")
        if typ == JSON:
            msg = json.loads(payload)
            t = msg.get("type")
            if t == "error":
                sys.exit("compositor error: " + msg.get("message", ""))
            if t == "hello":
                out["hello"] = msg
            elif t == "toplevel":
                out["toplevels"].append(msg)
            elif t == "surface":
                out["surfaces"].append(msg)
            elif t == "ready":
                break
        elif typ == COMMIT:
            f = struct.unpack(HEADER, payload[:88])
            header_size, flags, surface, window, seq = f[:5]
            rects, length = f[22], f[23]
            c = {"surface": surface, "window": window, "seq": seq, "flags": flags, "dataLength": length}
            c["received"] = len(payload) - header_size - 16 * rects
            out["commits"].append(c)
            if c["received"] < length:
                pending = c
        elif typ == COMMIT_DATA:
            (seq,) = struct.unpack(">I", payload[:4])
            if pending is None or pending["seq"] != seq:
                sys.exit(f"COMMIT_DATA for {seq} outside its commit")
            pending["received"] += len(payload) - 4
            if pending["received"] >= pending["dataLength"]:
                pending = None
    for c in out["commits"]:
        if c["received"] != c["dataLength"]:
            sys.exit(f"commit {c['seq']}: {c['received']} bytes, dataLength {c['dataLength']}")
    p.stdin.close()
    p.terminate()
    if want and not any(t.get("appId") == want for t in out["toplevels"]):
        sys.exit(f"no {want} toplevel in {out['toplevels']}")
    print(json.dumps(out))
  '';
in
  pkgs.testers.runNixOSTest {
    name = "pocketnix-gui";

    nodes.phone = {
      imports = [self.nixosModules.default ../example/configuration.nix];
      pocketnix.guest = {
        avf.enable = false;
        agent.listen = ["tcp:127.0.0.1:9100"];
        gui = {
          enable = true;
          appDefaults.foot.size = "compact";
        };
        terminal.profiles.env.command = ''printf 'wayland:%s ozone:%s tab:%s\n' "$WAYLAND_DISPLAY" "$NIXOS_OZONE_WL" "$POCKETNIX_TAB"'';
      };
      environment.systemPackages = [pkgs.foot];
      virtualisation.memorySize = 2048;
    };

    testScript = ''
      import json

      A = "${cli}/bin/pocketnix agent --connect tcp:127.0.0.1:9100"


      def user(cmd):
          return f"runuser -u nixos -- env XDG_RUNTIME_DIR=/run/user/1000 {cmd}"


      def replay():
          out = phone.wait_until_succeeds(f"timeout 30 ${probe}/bin/gui-probe --want-app foot {A} gui", timeout=120)
          return json.loads(out)


      start_all()
      phone.wait_for_unit("pocketnix-agent.service")
      phone.wait_for_open_port(9100)
      # Lingering starts it, and with it the sockets.
      phone.wait_for_unit("user@1000.service")
      phone.wait_for_unit("pocketnix-gui.socket", "nixos")
      phone.wait_for_unit("pocketnix-wayland.socket", "nixos")

      with subtest("the config push turns GUI on, with the module's app settings"):
          cfg = json.loads(phone.succeed(f"{A} config"))
          assert cfg["gui"] == {
              "enable": True,
              "defaults": {"size": "phone", "input": "touch"},
              "appDefaults": {"foot": {"size": "compact"}},
          }, cfg["gui"]

      with subtest("tabs and user services get the Wayland display"):
          out = phone.succeed(f"{A} exec --profile env")
          assert "wayland:wayland-0 ozone:1 tab:" in out, out
          assert "tab: " not in out, out
          env = phone.succeed(user("systemctl --user show-environment"))
          assert "WAYLAND_DISPLAY=wayland-0" in env, env
          assert "NIXOS_OZONE_WL=1" in env, env

      with subtest("a Wayland client starts the compositor"):
          phone.fail(user("systemctl --user is-active pocketnix-compositor.service"))
          # Gets WAYLAND_DISPLAY from the user manager, like any user service.
          phone.succeed(user("systemd-run --user --unit=foot foot"))
          phone.wait_for_unit("pocketnix-compositor.service", "nixos")

      with subtest("the gui stream reaches the compositor and gets the replay"):
          first = replay()
          assert first["hello"]["version"] == 1, first["hello"]
          assert first["hello"]["instance"], first["hello"]
          foot = next(t for t in first["toplevels"] if t["appId"] == "foot")
          full = [c for c in first["commits"] if c["window"] == foot["id"] and c["flags"] & 0b11 == 0b11]
          assert full and all(c["dataLength"] > 0 for c in full), first["commits"]

      with subtest("windows outlive the agent"):
          phone.succeed("systemctl restart pocketnix-agent.service")
          phone.wait_for_open_port(9100)
          again = replay()
          assert again["hello"]["instance"] == first["hello"]["instance"], again["hello"]
          assert any(t["id"] == foot["id"] and t["appId"] == "foot" for t in again["toplevels"]), again["toplevels"]
          phone.succeed(user("systemctl --user is-active foot.service"))
    '';
  }
