# A guest with gui.enable: socket activation brings up the compositor for a
# Wayland client, and the agent's gui stream reaches it and gets its replay,
# also after the agent restarts; the app list has the system profile's apps,
# a launch from it opens a window bound to the launch, and foot takes text
# from the IME and as typed keys.
{
  pkgs,
  self,
}: let
  cli = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix-cli;
  silence = pkgs.runCommand "silence.wav" {} "${pkgs.sox}/bin/sox -n -r 48000 -c 2 -t wav $out trim 0 30";
  # Speaks the app's side of docs/gui-protocol.md through the command it's
  # given (`pocketnix agent … gui`) up to `ready`, and prints what the replay
  # held as JSON. Fails without a toplevel of --want-app. With --launch, it
  # then launches that entry and waits for its window and its icon. With
  # --wait-app, it says "ready" on stderr and waits for an app list pushed
  # after the replay that has that entry. With --type, it focuses that app's
  # window, presents its commits, commits "héllo " through its text input
  # and then types "wörld\n", waiting each time for the client to move its
  # cursor.
  probe = pkgs.writers.writePython3Bin "gui-probe" {flakeIgnore = ["E501"];} ''
    import json
    import struct
    import subprocess
    import sys

    JSON, COMMIT, COMMIT_DATA, ICON, PRESENTED = 0x00, 0x01, 0x02, 0x03, 0x10
    GROUPED = 1 << 4
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
    want = launch = wait_app = type_app = None
    while args[:1] in (["--want-app"], ["--launch"], ["--wait-app"], ["--type"]):
        if args[0] == "--want-app":
            want = args[1]
        elif args[0] == "--wait-app":
            wait_app = args[1]
        elif args[0] == "--type":
            type_app = args[1]
        else:
            launch = args[1]
        args = args[2:]
    p = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    send_json(p.stdin, {"type": "hello", "version": 2, "minVersion": 2, "app": "gui-probe", "hashes": False})
    out = {"toplevels": [], "surfaces": [], "commits": []}
    pending = None
    ready = False
    typing = {"window": 0, "stage": 0, "rect": None}


    def focus(window):
        typing["window"] = window
        send_json(p.stdin, {"type": "window", "id": window, "width": 0, "height": 0, "scale": 1.0,
                            "focused": True, "visible": True, "maximized": False, "fullscreen": False})


    def text_input(msg):
        if not typing["window"] or msg["window"] != typing["window"] or msg["session"] == 0:
            return
        if typing["stage"] == 0:
            out["textInput"] = msg
            send_json(p.stdin, {"type": "ime", "session": msg["session"], "serial": 1, "commit": "héllo "})
            typing["stage"] = 1
        elif typing["stage"] == 1 and msg["applied"] >= 1:
            typing["rect"] = msg["cursorRect"]
            send_json(p.stdin, {"type": "text", "window": typing["window"], "text": "wörld\n"})
            typing["stage"] = 2
        elif typing["stage"] == 2 and msg["cursorRect"] != typing["rect"]:
            out["typed"] = True


    def done():
        if not ready:
            return False
        if type_app:
            return "typed" in out
        if wait_app:
            return "pushed" in out
        if not launch:
            return True
        return "launched" in out and "icon" in out


    while not done():
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
                if launch and ready and msg.get("launch") == "vm-test":
                    out["launched"] = msg
                if type_app and ready and not typing["window"] and msg.get("appId") == type_app:
                    focus(msg["id"])
            elif t == "text-input":
                text_input(msg)
            elif t == "surface":
                out["surfaces"].append(msg)
            elif t == "apps":
                out["apps"] = msg["apps"]
                if ready and any(a["id"] == wait_app for a in msg["apps"]):
                    out["pushed"] = True
            elif t == "launch-failed":
                sys.exit(f"launch failed: {msg}")
            elif t == "ready":
                ready = True
                if type_app:
                    top = next((x for x in out["toplevels"] if x["appId"] == type_app), None)
                    if top:
                        focus(top["id"])
                if wait_app:
                    print("ready", file=sys.stderr, flush=True)
                if launch:
                    entry = next((a for a in out.get("apps", []) if a["id"] == launch), None)
                    if entry is None:
                        sys.exit(f"no {launch} in the app list: {out.get('apps')}")
                    send_json(p.stdin, {"type": "launch", "id": "vm-test", "app": launch, "action": "", "tab": ""})
                    send_json(p.stdin, {"type": "icon", "icon": entry["icon"], "size": 64})
        elif typ == ICON:
            size, key_len = struct.unpack(">HB", payload[:3])
            out["icon"] = {"size": size, "key": payload[3:3 + key_len].decode(), "png": payload[3 + key_len:3 + key_len + 24].hex()}
        elif typ == COMMIT:
            f = struct.unpack(HEADER, payload[:88])
            header_size, flags, surface, window, seq = f[:5]
            rects, length = f[22], f[23]
            c = {"surface": surface, "window": window, "seq": seq, "flags": flags, "dataLength": length}
            c["received"] = len(payload) - header_size - 16 * rects
            out["commits"].append(c)
            # Typing needs the client to redraw, which waits for its frame callbacks.
            if type_app and not flags & GROUPED:
                p.stdin.write(struct.pack(">BIII", PRESENTED, 8, window, seq))
                p.stdin.flush()
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
    # gui.enable sets an overlay.
    node.pkgsReadOnly = false;

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
      # The phone's sound card, played nowhere.
      virtualisation.qemu.options = ["-audiodev none,id=snd0" "-device virtio-sound-pci,audiodev=snd0"];
      # Test VMs leave it out unless asked; the switch subtest needs it.
      system.switch.enable = true;
    };

    testScript = ''
      import json
      import time

      A = "${cli}/bin/pocketnix agent --connect tcp:127.0.0.1:9100"


      def user(cmd):
          return f"runuser -u nixos -- env XDG_RUNTIME_DIR=/run/user/1000 {cmd}"


      def replay(*flags):
          out = phone.wait_until_succeeds(f"timeout 60 ${probe}/bin/gui-probe {' '.join(flags)} {A} gui", timeout=180)
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

      with subtest("a switch brings GUI up in a user manager that's already running"):
          # As before a switch that turns GUI on: no sockets, no Wayland display.
          phone.succeed(user("systemctl --user stop pocketnix-gui.socket pocketnix-wayland.socket"))
          phone.succeed(user("systemctl --user unset-environment WAYLAND_DISPLAY NIXOS_OZONE_WL"))
          phone.succeed("/run/current-system/bin/switch-to-configuration test")
          phone.wait_for_unit("pocketnix-gui.socket", "nixos")
          phone.wait_for_unit("pocketnix-wayland.socket", "nixos")
          env = phone.succeed(user("systemctl --user show-environment"))
          assert "WAYLAND_DISPLAY=wayland-0" in env, env
          assert "NIXOS_OZONE_WL=1" in env, env

      with subtest("a Wayland client starts the compositor"):
          phone.fail(user("systemctl --user is-active pocketnix-compositor.service"))
          # Gets WAYLAND_DISPLAY from the user manager, like any user service.
          phone.succeed(user("systemd-run --user --unit=foot foot"))
          phone.wait_for_unit("pocketnix-compositor.service", "nixos")

      with subtest("the user's apps get sound, SVG images and the FileChooser portal"):
          # The user has no seat session: PipeWire finds the card only through the audio group.
          phone.wait_until_succeeds(user("wpctl inspect @DEFAULT_AUDIO_SINK@"), timeout=60)
          phone.succeed(user("systemd-run --user --unit=silence ${pkgs.pipewire}/bin/pw-play ${silence}"))
          phone.wait_until_succeeds("grep -x 'format: S16_LE' /proc/asound/card0/pcm0p/sub0/hw_params", timeout=30)
          phone.succeed("printf '<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"8\" height=\"8\"/>' >/tmp/dot.svg")
          phone.succeed(user("bash -lc '${pkgs.gdk-pixbuf}/bin/gdk-pixbuf-thumbnailer -s 8 /tmp/dot.svg /tmp/dot.png'"))
          phone.succeed("test -s /tmp/dot.png")
          # Activates xdg-desktop-portal, which exports FileChooser only if the GTK portal implements it.
          phone.wait_until_succeeds(user(
              "busctl --user get-property org.freedesktop.portal.Desktop /org/freedesktop/portal/desktop org.freedesktop.portal.FileChooser version"
          ), timeout=60)

      with subtest("the gui stream reaches the compositor and gets the replay"):
          first = replay("--want-app", "foot")
          assert first["hello"]["version"] == 2, first["hello"]
          assert first["hello"]["instance"], first["hello"]
          foot = next(t for t in first["toplevels"] if t["appId"] == "foot")
          full = [c for c in first["commits"] if c["window"] == foot["id"] and c["flags"] & 0b11 == 0b11]
          assert full and all(c["dataLength"] > 0 for c in full), first["commits"]

      with subtest("windows outlive the agent"):
          phone.succeed("systemctl restart pocketnix-agent.service")
          phone.wait_for_open_port(9100)
          again = replay("--want-app", "foot")
          assert again["hello"]["instance"] == first["hello"]["instance"], again["hello"]
          assert any(t["id"] == foot["id"] and t["appId"] == "foot" for t in again["toplevels"]), again["toplevels"]
          phone.succeed(user("systemctl --user is-active foot.service"))

      with subtest("the app list has the system profile's apps, and a launch from it opens one"):
          launched = replay("--want-app", "foot", "--launch", "foot")
          apps = {a["id"]: a for a in launched["apps"]}
          assert apps.get("foot", {}).get("icon"), sorted(apps)
          # The window started by hand is named after its entry too.
          assert next(t for t in launched["toplevels"] if t["id"] == foot["id"])["app"] == "foot", launched["toplevels"]
          win = launched["launched"]
          assert (win["app"], win["appId"], win["tab"], win["window"]) == ("foot", "foot", "", win["id"]), win
          assert win["id"] != foot["id"], win
          icon = launched["icon"]
          assert (icon["key"], icon["size"]) == (apps["foot"]["icon"], 64), icon
          png = bytes.fromhex(icon["png"])
          assert png[:8] == b"\x89PNG\r\n\x1a\n" and png[16:24] == (64).to_bytes(4, "big") * 2, icon
          scopes = phone.succeed(user("systemctl --user list-units --plain --no-legend --state=running 'app-pocketnix-foot-*.scope'"))
          assert scopes.startswith("app-pocketnix-foot-"), scopes
          slice = phone.succeed(user(f"systemctl --user show -P Slice {scopes.split()[0]}")).strip()
          assert slice == "app.slice", slice

      with subtest("a reload rescans the app list and keeps the windows"):
          apps = "/home/nixos/.local/share/applications"
          phone.succeed(user(f"mkdir -p {apps}"))
          # Lets the rescan the mkdir scheduled run first: each waits out a 1 s debounce.
          time.sleep(3)
          # Every replay rescans, so the probe stays connected through the mount and the reload.
          phone.succeed(f"${probe}/bin/gui-probe --wait-app pocketnix-mounted {A} gui >/tmp/pushed.json 2>/tmp/probe.err &")
          phone.wait_until_succeeds("grep -q ready /tmp/probe.err")
          # A mount makes no inotify event: only a rescan finds what it brings.
          phone.succeed("mkdir /tmp/apps")
          phone.succeed("printf '[Desktop Entry]\\nType=Application\\nName=Mounted\\nExec=foot\\n' >/tmp/apps/pocketnix-mounted.desktop")
          phone.succeed(f"chmod -R a+rX /tmp/apps && mount --bind /tmp/apps {apps}")
          time.sleep(3)
          phone.fail("test -s /tmp/pushed.json")
          pid = phone.succeed(user("systemctl --user show -P MainPID pocketnix-compositor.service")).strip()
          phone.succeed(user("systemctl --user reload pocketnix-compositor.service"))
          phone.wait_until_succeeds("test -s /tmp/pushed.json", timeout=30)
          after = phone.succeed(user("systemctl --user show -P MainPID pocketnix-compositor.service")).strip()
          assert after == pid, (pid, after)
          assert any(t["id"] == foot["id"] for t in replay()["toplevels"])
          phone.succeed(f"umount {apps}")

      with subtest("foot takes text from the IME and as typed keys"):
          phone.succeed(user("systemd-run --user --unit=typed foot --app-id=typed sh -c 'cat >/home/nixos/typed'"))
          typed = replay("--want-app", "typed", "--type", "typed")
          ti = typed["textInput"]
          assert ti["purpose"] == "terminal" and ti["session"] >= 1, ti
          phone.wait_until_succeeds("grep -q wörld /home/nixos/typed", timeout=30)
          content = phone.succeed("cat /home/nixos/typed")
          assert content == "héllo wörld\n", repr(content)
    '';
  }
