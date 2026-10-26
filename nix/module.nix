# What a NixOS guest provides to the pocketnix app: the agent the app dials
# over vsock with the terminal configuration it serves, and (avf.enable) the
# AVF platform: Google's guest kernel, the raw disk image u-boot boots,
# first-boot setup and a login on the hvc0 console (the app's recovery tab).
{self}: {
  config,
  lib,
  pkgs,
  extendModules,
  modulesPath,
  ...
}: let
  cfg = config.pocketnix.guest;
  inherit (lib) mkOption mkEnableOption mkIf mkDefault mkMerge types;

  extraKey = types.submodule {
    options = {
      key = mkOption {
        type = types.str;
        description = "Termux extra-keys name (ESC, TAB, CTRL, ALT, UP, PGUP, F1, …) or a literal character.";
      };
      popup = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Key sent on a swipe up from this one.";
      };
      label = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Text shown on the button instead of the key's name.";
      };
    };
  };

  agentConfig =
    {
      user = cfg.user;
      hostname = config.networking.hostName;
      forwards =
        lib.mapAttrsToList (name: f: {
          inherit name;
          inherit (f) guestPort hostPort lan protocol;
        })
        cfg.forwards;
      terminal = {
        profiles = lib.mapAttrsToList (name: p: {inherit name;} // lib.optionalAttrs (p.command != null) {inherit (p) command;}) cfg.terminal.profiles;
        inherit (cfg.terminal) defaultProfile;
        extraKeys = map (map (k: lib.filterAttrs (_: v: v != null) k)) cfg.terminal.extraKeys;
        theme =
          {inherit (cfg.terminal.theme) colors;}
          // lib.optionalAttrs (fontFile != null) {font = "${fontFile}";};
        snippets =
          lib.mapAttrsToList (name: s: {
            inherit name;
            inherit (s) command submit;
          })
          cfg.terminal.snippets;
      };
      updates =
        lib.optionalAttrs (upd.buildHost.host != null) {
          buildHost =
            {inherit (upd.buildHost) host installable;}
            // lib.optionalAttrs (upd.buildHost.sshKey != null) {inherit (upd.buildHost) sshKey;};
        }
        // lib.optionalAttrs (upd.published.url != null) {published = {inherit (upd.published) url;};}
        // lib.optionalAttrs (upd.checkInterval != null) {inherit (upd) checkInterval;};
      share = lib.filterAttrs (_: v: v != null) {inherit (cfg.share) directory command;};
    }
    // lib.optionalAttrs (cfg.terminal.historyFile != null) {history = cfg.terminal.historyFile;}
    // lib.optionalAttrs (cfg.files.root != null) {filesRoot = cfg.files.root;};
  upd = cfg.updates;

  theme = cfg.terminal.theme;
  # The app takes one file; a family resolves to its file in the package.
  fontFile =
    if theme.font != null
    then theme.font
    else if theme.fontFamily == null
    then null
    else
      pkgs.runCommand "pocketnix-terminal-font" {nativeBuildInputs = [pkgs.fontconfig];} ''
        export FONTCONFIG_FILE=${pkgs.makeFontsConf {fontDirectories = [theme.fontFamily.package];}} HOME=$TMPDIR
        file=$(fc-match -f '%{file}' ${lib.escapeShellArg "${theme.fontFamily.family}:style=${theme.fontFamily.style}"})
        case $file in ${theme.fontFamily.package}/*) ;; *) echo "no ${lib.escapeShellArg theme.fontFamily.family} in ${theme.fontFamily.package} (fontconfig offered $file)" >&2; exit 1 ;; esac
        cp "$file" $out
      '';

  # Only the guest command on PATH, not the agent or the host CLI.
  guestTools = pkgs.runCommand "pocketnix-guest-tools" {} ''
    mkdir -p $out/bin
    ln -s ${lib.getExe' cfg.package "pocketnix-guest"} $out/bin/pocketnix-guest
    ${lib.optionalString cfg.integrations.xdgOpen "ln -s ${lib.getExe' cfg.package "pocketnix-guest"} $out/bin/xdg-open"}
  '';
  guestExe = lib.getExe' cfg.package "pocketnix-guest";

  notifydUnit = "pocketnix-notifyd.service";
  notifydService = pkgs.writeTextDir "share/dbus-1/services/org.freedesktop.Notifications.service" ''
    [D-BUS Service]
    Name=org.freedesktop.Notifications
    Exec=${guestExe} notifyd
    SystemdService=${notifydUnit}
  '';

  # What Android opens instead of the guest (integrations.xdgOpen).
  openTypes =
    map (s: "x-scheme-handler/${s}") ["http" "https" "mailto" "tel" "geo"]
    ++ ["text/html" "application/pdf" "image/png" "image/jpeg" "image/gif" "image/webp" "image/svg+xml" "video/mp4" "video/webm" "audio/mpeg" "audio/ogg" "audio/flac"];
  openDesktop = pkgs.writeTextDir "share/applications/pocketnix-open.desktop" ''
    [Desktop Entry]
    Type=Application
    Name=Open on Android
    Exec=${guestExe} open %u
    MimeType=${lib.concatMapStrings (t: "${t};") openTypes}
    NoDisplay=true
  '';
  agentConfigFile = (pkgs.formats.json {}).generate "pocketnix-agent.json" agentConfig;

  # Mirrors systemd-boot-builder.py's copy_from_file naming, so the first
  # `switch` adopts generation 1's files instead of leaving strays behind.
  efiName = path: builtins.unsafeDiscardStringContext "/EFI/nixos/${baseNameOf (dirOf path)}-${baseNameOf path}.efi";

  # extendModules keeps nixpkgs' repart module (it sets system.build.image
  # unconditionally) out of the guest's own system.
  image =
    (extendModules {
      modules = [
        (modulesPath + "/image/repart.nix")
        ({config, ...}: let
          inherit (config.system.build) toplevel;
          kernel = "${config.boot.kernelPackages.kernel}/${config.system.boot.loader.kernelFile}";
          initrd = "${config.system.build.initialRamdisk}/${config.system.boot.loader.initrdFile}";
          efiArch = pkgs.stdenv.hostPlatform.efiArch;
          systemdBoot = "${pkgs.systemd}/lib/systemd/boot/efi/systemd-boot${efiArch}.efi";
        in {
          image.repart = {
            name = "${config.networking.hostName}-pocketnix";
            sectorSize = 512;
            partitions = {
              "10-esp" = {
                contents = {
                  # u-boot runs the removable path; bootctl (every switch) wants the EFI/systemd copy.
                  "/EFI/BOOT/BOOT${lib.toUpper efiArch}.EFI".source = systemdBoot;
                  "/EFI/systemd/systemd-boot${efiArch}.efi".source = systemdBoot;
                  ${efiName kernel}.source = kernel;
                  ${efiName initrd}.source = initrd;
                  "/loader/loader.conf".source = pkgs.writeText "loader.conf" ''
                    timeout ${toString config.boot.loader.timeout}
                    default nixos-generation-1.conf
                  '';
                  "/loader/entries/nixos-generation-1.conf".source = pkgs.writeText "nixos-generation-1.conf" ''
                    title NixOS
                    version Generation 1
                    linux ${efiName kernel}
                    initrd ${efiName initrd}
                    options init=${toplevel}/init ${toString config.boot.kernelParams}
                  '';
                };
                repartConfig = {
                  Type = "esp";
                  Format = "vfat";
                  Label = "ESP";
                  # Room for several generations' kernel + initrd.
                  SizeMinBytes = "512M";
                };
              };
              "20-root" = {
                storePaths = [toplevel];
                contents."/nix-path-registration".source = "${pkgs.closureInfo {rootPaths = [toplevel];}}/registration";
                repartConfig = {
                  Type = "root";
                  Format = "ext4";
                  Label = "nixos";
                  Minimize = "guess";
                };
              };
            };
          };
        })
      ];
    }).config.system.build.image;

  # The same image assembled by another system's tools: systemd-repart needs
  # `unshare --map-root-user`, which fails under multithreaded qemu-user, so a
  # binfmt host must assemble it natively.
  imageArgs = removeAttrs (lib.functionArgs (import (modulesPath + "/image/repart-image.nix"))) [
    "name"
    "version"
    "baseName"
    "compression"
    "fileSystems"
    "finalPartitions"
    "split"
    "seed"
    "definitionsDirectory"
    "imageSize"
    "sectorSize"
    "mkfsEnv"
    "createEmpty"
  ];
  imageBuiltOn = system:
    if system == pkgs.stdenv.buildPlatform.system
    then image
    else
      (image.override (lib.intersectAttrs imageArgs (import pkgs.path {inherit system;}))).overrideAttrs {
        # The override's stdenv sets --architecture, which picks the root partition's GPT type.
        inherit (image) systemdRepartFlags;
        # Host-tool repart's CopyFiles= follows a store path that is itself a
        # symlink and copies its target.
        postPatch = ''
          chmod u+w $finalRepartDefinitions/
          for conf in $finalRepartDefinitions/*.conf; do
            links=$(sed -n 's|^CopyFiles=\(/nix/store/[^:]*\).*|\1|p' "$conf" | while read -r p; do if [ -L "$p" ]; then echo "$p"; fi; done)
            for p in $links; do
              sed -i "s|^CopyFiles=$p\(:$p\)\?\$|MakeSymlinks=$p:$(readlink "$p")|" "$conf"
            done
          done
        '';
      };

  # What the app's theme takes (TerminalTheme.kt); it drops anything else.
  colorKey = "foreground|background|cursor|color([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])";
  # time.ParseDuration's syntax without a sign: the agent refuses negative intervals.
  goDuration = "0|(([0-9]+(\\.[0-9]*)?|\\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+";
  # TCP and UDP ports are separate spaces: a forward of each may share a number.
  hostPorts = lib.mapAttrsToList (_: f: "${f.protocol}/${toString f.hostPort}") cfg.forwards;
  badColors = lib.filter (k: builtins.match colorKey k == null) (lib.attrNames theme.colors);
in {
  imports = [
    (lib.mkRenamedOptionModule ["pocketnix" "guest" "integrations" "notifySend"] ["pocketnix" "guest" "integrations" "notifications"])
  ];

  options.pocketnix.guest = {
    enable = mkEnableOption "the pocketnix guest side";
    package = mkOption {
      type = types.package;
      # This flake's build, whose nixpkgs has the Go that go.mod wants; when
      # cross-compiling, this nixpkgs' build, as Go cross-compiles natively.
      default =
        if pkgs.stdenv.buildPlatform == pkgs.stdenv.hostPlatform && self.packages ? ${pkgs.stdenv.hostPlatform.system}
        then self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix
        else pkgs.callPackage ./go.nix {};
      defaultText = lib.literalExpression "pocketnix.packages.\${system}.pocketnix";
      description = "The pocketnix package providing `pocketnix-agent`.";
    };
    user = mkOption {
      type = types.str;
      default = "root";
      example = "alice";
      description = "User that the app's terminal tabs run as, and that the hvc0 console logs in.";
    };

    agent.listen = mkOption {
      type = types.listOf types.str;
      default = ["vsock:9100"];
      example = ["vsock:9100" "tcp:0.0.0.0:9100"];
      description = ''
        Addresses the agent serves. Only vsock is limited to the app: a TCP
        listener hands a shell to anyone who reaches it, so add one only for
        tests.
      '';
    };

    forwards = mkOption {
      default = {};
      example = lib.literalExpression ''
        {
          ssh = { guestPort = 22; hostPort = 2222; };
          mosh = { guestPort = 60001; protocol = "udp"; };
        }
      '';
      description = "Ports the app exposes on the phone, on 127.0.0.1 unless `lan` is set.";
      type = types.attrsOf (types.submodule ({config, ...}: {
        options = {
          guestPort = mkOption {
            type = types.port;
            description = "Port on the guest's 127.0.0.1.";
          };
          hostPort = mkOption {
            type = types.port;
            default = config.guestPort;
            defaultText = lib.literalExpression "guestPort";
            description = "Port the app listens on.";
          };
          lan = mkOption {
            type = types.bool;
            default = false;
            description = "Listen on every interface instead of 127.0.0.1.";
          };
          protocol = mkOption {
            type = types.enum ["tcp" "udp"];
            default = "tcp";
            description = ''
              What to forward. A UDP forward relays each phone-side client
              address over a stream of its own, dropped after 2 minutes of
              silence; it needs an app and agent that know UDP forwards.
            '';
          };
        };
      }));
    };

    terminal = {
      profiles = mkOption {
        default = {shell = {};};
        description = "Kinds of terminal tab; each runs through the user's login shell.";
        type = types.attrsOf (types.submodule {
          options.command = mkOption {
            type = types.nullOr types.str;
            default = null;
            example = "tmux new-session -A -s main";
            description = "Command the tab runs (`$SHELL -l -c`); null runs the login shell itself.";
          };
        });
      };
      defaultProfile = mkOption {
        type = types.str;
        default = "shell";
        description = "Profile a new tab runs.";
      };
      extraKeys = mkOption {
        type = types.listOf (types.listOf (types.coercedTo types.str (key: {inherit key;}) extraKey));
        default = [
          [
            "ESC"
            {
              key = "/";
              popup = "\\";
            }
            {
              key = "-";
              popup = "|";
            }
            "HOME"
            "UP"
            "END"
            "PGUP"
          ]
          ["TAB" "CTRL" "ALT" "LEFT" "DOWN" "RIGHT" "PGDN"]
        ];
        description = "Rows of keys above the keyboard, in Termux's extra-keys vocabulary.";
      };
      theme = {
        colors = mkOption {
          type = types.attrsOf (types.strMatching "#[0-9a-fA-F]{6}");
          default = {};
          example = {
            background = "#1e1e2e";
            foreground = "#cdd6f4";
            color1 = "#f38ba8";
          };
          description = "Terminal colors by Termux property name: foreground, background, cursor, color0 to color15.";
        };
        font = mkOption {
          type = types.nullOr types.path;
          default = null;
          example = lib.literalExpression ''"''${pkgs.jetbrains-mono}/share/fonts/truetype/JetBrainsMono-Regular.ttf"'';
          description = "TTF or OTF file the terminal renders with.";
        };
        fontFamily = mkOption {
          default = null;
          example = lib.literalExpression ''{ package = pkgs.nerd-fonts.jetbrains-mono; family = "JetBrainsMono Nerd Font Mono"; }'';
          description = "A font by family, found in its package with fontconfig; for `font` when you think in families.";
          type = types.nullOr (types.submodule {
            options = {
              package = mkOption {
                type = types.package;
                description = "Package holding the family's files.";
              };
              family = mkOption {
                type = types.str;
                description = "fontconfig family name.";
              };
              style = mkOption {
                type = types.str;
                default = "Regular";
                description = "fontconfig style.";
              };
            };
          });
        };
      };
      snippets = mkOption {
        default = {};
        example = {
          "Follow the journal" = "journalctl -f";
          "Attach zellij" = {
            command = "zellij attach --create main";
            submit = true;
          };
        };
        description = "Command-palette entries, listed above the shell history. A string is the command.";
        type = types.attrsOf (types.coercedTo types.str (command: {inherit command;}) (types.submodule {
          options = {
            command = mkOption {
              type = types.str;
              description = "Text typed into the tab.";
            };
            submit = mkOption {
              type = types.bool;
              default = false;
              description = "Press Enter after typing the command.";
            };
          };
        }));
      };
      historyFile = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "/home/alice/.local/share/zsh/history";
        description = ''
          Shell history the command palette offers. Null reads the login
          shell's default file (bash, zsh or fish); zsh files are recognized
          by "zsh" in their name, fish files by ending in `fish_history`.
        '';
      };
    };

    updates = {
      buildHost = {
        host = mkOption {
          type = types.nullOr types.str;
          default = null;
          example = "alice@desktop";
          description = ''
            ssh destination of a build host: a machine with Nix that evaluates
            and builds this system, which the guest then copies (or
            substitutes, when the host pushes to a cache the guest uses).
            Port and the like belong in `programs.ssh.extraConfig`, and its
            host key in `programs.ssh.knownHosts`: the agent never prompts.
          '';
        };
        flake = mkOption {
          type = types.nullOr types.str;
          default = null;
          example = "github:alice/nixos";
          description = "Flake whose `nixosConfigurations.<hostName>` is this system; the build host refreshes it on every check.";
        };
        installable = mkOption {
          type = types.nullOr types.str;
          default =
            if upd.buildHost.flake == null
            then null
            else "${upd.buildHost.flake}#nixosConfigurations.${config.networking.hostName}.config.system.build.toplevel";
          defaultText = lib.literalExpression ''"''${flake}#nixosConfigurations.''${networking.hostName}.config.system.build.toplevel"'';
          description = "What the build host builds (`nix build --refresh <installable>`).";
        };
        sshKey = mkOption {
          type = types.nullOr types.str;
          default = "/etc/ssh/ssh_host_ed25519_key";
          description = "Identity the agent (as root) uses for the build host; null leaves it to ssh. The build host's user must trust its public key.";
        };
      };
      published.url = mkOption {
        type = types.nullOr (types.strMatching "https://.+");
        default = null;
        example = "https://ci.example.com/phone/toplevel";
        description = ''
          URL whose body is this system's toplevel store path, which CI
          publishes after pushing the closure to a binary cache the guest
          trusts. The guest only substitutes it. With a build host too, this
          is the fallback while the build host can't be reached.
        '';
      };
      checkInterval = mkOption {
        type = types.nullOr (types.strMatching goDuration);
        default = "6h";
        example = "30m";
        description = "How often the agent checks for a newer system in the background (a Go duration); null checks only when asked in the app.";
      };
    };

    integrations = {
      xdgOpen = mkOption {
        type = types.bool;
        default = true;
        description = ''
          Put an `xdg-open` on PATH (and default `$BROWSER` to it) that opens
          URLs, and files under `files.root`, on Android; and make Android the
          default (`xdg.mime.defaultApplications`) for web, mail, phone and map
          links and a few common file types, so `gio open` and GLib/Qt
          programs open there too.
        '';
      };
      notifications = mkOption {
        type = types.bool;
        default = true;
        description = ''
          Serve `org.freedesktop.Notifications` on the user's session bus
          (D-Bus-activated `pocketnix-guest notifyd`), so desktop notifications
          and libnotify's `notify-send` (installed) post Android notifications,
          with actions. `pocketnix-guest notify` works either way, also for
          root and system units, which have no session bus.
        '';
      };
    };

    share = {
      directory = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "Downloads";
        description = "Where Android's share sheet drops text and files: absolute, or relative to the user's home. Null is `~/Inbox`.";
      };
      command = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "notify-send shared";
        description = "Run (by /bin/sh, as `pocketnix.guest.user`) on each delivered file, its path the last argument.";
      };
    };

    files.root = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "/home/alice/shared";
      description = "Directory the Files app and Android's file pickers see, read-write. Null is the user's home.";
    };

    avf.enable = mkOption {
      type = types.bool;
      default = true;
      description = "The AVF platform: kernel, disk image, boot loader, first boot and the hvc0 console. Off for other hypervisors, such as NixOS tests.";
    };
    # Off only in the evaluation that yields the unwrapped installer.
    avf.wrapBootLoader = mkOption {
      type = types.bool;
      default = true;
      internal = true;
      visible = false;
    };
  };

  options.system.build.pocketnixImage = mkOption {
    type = types.attrsOf types.package;
    readOnly = true;
    description = "Raw GPT disk image for the pocketnix app, keyed by the system that assembles it.";
  };

  config = mkIf cfg.enable (mkMerge [
    {
      assertions = [
        {
          assertion = cfg.terminal.profiles ? ${cfg.terminal.defaultProfile};
          message = "pocketnix.guest.terminal.defaultProfile `${cfg.terminal.defaultProfile}` is not a profile";
        }
        {
          assertion = theme.font == null || theme.fontFamily == null;
          message = "pocketnix.guest.terminal.theme: set font or fontFamily, not both";
        }
        {
          assertion = upd.buildHost.host != null -> upd.buildHost.installable != null;
          message = "pocketnix.guest.updates.buildHost.host needs buildHost.flake (or buildHost.installable)";
        }
        {
          assertion = upd.buildHost.host != null && upd.buildHost.sshKey == "/etc/ssh/ssh_host_ed25519_key" -> config.services.openssh.enable;
          message = "pocketnix.guest.updates.buildHost: the default sshKey is sshd's host key; enable services.openssh or set buildHost.sshKey";
        }
        {
          assertion = cfg.user == "root" || config.users.users ? ${cfg.user};
          message = "pocketnix.guest.user `${cfg.user}` is not in users.users";
        }
        {
          assertion = lib.allUnique hostPorts;
          message = "pocketnix.guest.forwards: two forwards of one protocol share a hostPort";
        }
        {
          assertion = badColors == [];
          message = "pocketnix.guest.terminal.theme.colors: unknown ${toString badColors} (foreground, background, cursor, color0 to color255)";
        }
        {
          assertion = cfg.avf.enable -> pkgs.stdenv.hostPlatform.isAarch64;
          message = "pocketnix.guest.avf.enable needs an aarch64-linux guest (Google's kernel is arm64); turn it off for other hypervisors";
        }
      ];
      warnings = lib.optional (cfg.avf.enable && lib.any (lib.hasPrefix "tcp:") cfg.agent.listen) "pocketnix.guest.agent.listen has a TCP listener on an AVF guest: anyone who reaches it gets a shell";

      boot.kernelModules = ["vmw_vsock_virtio_transport"];
      environment.etc."pocketnix/agent.json".source = agentConfigFile;
      # Above xdg-utils, which a desktop-ish config may pull in.
      environment.systemPackages = [(lib.hiPrio guestTools)];
      environment.sessionVariables.BROWSER = mkIf cfg.integrations.xdgOpen (mkDefault "xdg-open");

      systemd.services.pocketnix-agent = {
        description = "pocketnix guest agent";
        wantedBy = ["multi-user.target"];
        after = ["systemd-modules-load.service"];
        # Terminal tabs run under the agent, and a switch is usually run from
        # one: restarting the agent mid-switch would kill the switch. Config
        # changes reload instead, and reach the app live.
        restartIfChanged = false;
        reloadTriggers = [agentConfigFile];
        # Updates: ssh to the build host, nix to fetch, systemd-run to switch.
        path = [config.nix.package config.programs.ssh.package config.systemd.package];
        serviceConfig = {
          ExecStart = lib.escapeShellArgs ([(lib.getExe' cfg.package "pocketnix-agent") "--guest-socket" "/run/pocketnix/guest.sock" "--state-dir" "/var/lib/pocketnix"] ++ lib.concatMap (a: ["--listen" a]) cfg.agent.listen);
          RuntimeDirectory = "pocketnix";
          RuntimeDirectoryMode = "0755";
          StateDirectory = "pocketnix";
          StateDirectoryMode = "0700";
          ExecReload = "${lib.getExe' pkgs.coreutils "kill"} -HUP $MAINPID";
          Restart = "always";
          RestartSec = 1;
          # The app's only way into a wedged guest besides the console.
          OOMScoreAdjust = -1000;
        };
      };
    }

    (mkIf cfg.avf.enable {
      system.build.pocketnixImage = lib.genAttrs ["aarch64-linux" "x86_64-linux"] imageBuiltOn;

      boot.kernelPackages = mkDefault (pkgs.linuxPackagesFor (pkgs.callPackage ./avf-kernel/package.nix {}));
      boot.kernelParams = [
        "console=hvc0"
        # Pixel 8 Pro's hypervisor advertises MPAM but traps MPAMIDR_EL1: an
        # oops in init_cpu_features at t=0 with more than one vCPU.
        "arm64.nompam"
        # Each guest timer wakes a vCPU thread on the phone: no lockup
        # detectors (an hrtimer per vCPU), and let idle timers batch.
        "nowatchdog"
        "kfence.deferrable=1"
        "rcutree.enable_rcu_lazy=1"
        "workqueue.power_efficient=1"
      ];
      # The defaults name PC storage/USB drivers this kernel doesn't ship; virtio is built in.
      boot.initrd.includeDefaultModules = false;
      # Other modules still ask for drivers (tpm-crb, …) this fixed kernel lacks.
      boot.initrd.allowMissingModules = true;
      boot.initrd.availableKernelModules = ["virtio_pci" "virtio_blk"];
      # Plain ext4 on virtio, and no TPM in the kernel: stage 1 needs neither
      # dbus (which copies in the whole systemd package), lvm2 nor tpm2-tss.
      boot.initrd.systemd.dbus.enable = mkDefault false;
      boot.initrd.systemd.tpm2.enable = mkDefault false;
      systemd.tpm2.enable = mkDefault false;
      services.lvm.enable = mkDefault false;

      boot.loader.systemd-boot.enable = true;
      # u-boot's EFI variables don't persist; systemd-boot lives at the removable path.
      boot.loader.efi.canTouchEfiVariables = false;
      # The app's boot-menu start taps a key on hvc0 to show the menu.
      boot.loader.timeout = mkDefault 0;
      # Google's kernel has no efivarfs, yet /sys/firmware/efi exists, so the
      # version check a plain `switch` runs (`bootctl status`, which reads EFI
      # variables) exits 1 and fails the switch. The install path passes
      # --no-variables: take it on every switch, as --install-bootloader does.
      system.build.installBootLoader = mkIf cfg.avf.wrapBootLoader (lib.mkForce (pkgs.writeShellScript "install-bootloader-pocketnix" ''
        export NIXOS_INSTALL_BOOTLOADER=1
        exec ${(extendModules {modules = [{pocketnix.guest.avf.wrapBootLoader = false;}];}).config.system.build.installBootLoader} "$@"
      ''));

      fileSystems."/" = {
        device = "/dev/disk/by-label/nixos";
        fsType = "ext4";
        # The app grows the image file to the configured disk size.
        autoResize = true;
      };
      fileSystems."/boot" = {
        device = "/dev/disk/by-label/ESP";
        fsType = "vfat";
        options = ["umask=0077"];
      };
      boot.growPartition = true;

      # Google's kernel brings dummy, ifb and tunnel devices; dhcpcd waiting
      # on them was the longest unit on phone-vm's boot (7 of 18 s). Only the
      # virtio NIC gets DHCP, without an ARP probe of its lease.
      networking.dhcpcd.allowInterfaces = mkDefault ["en*" "eth*"];
      networking.dhcpcd.denyInterfaces = ["dummy*" "ifb*"];
      networking.dhcpcd.extraConfig = "noarp";

      # The closure is pushed over adb, and an update copies what changed:
      # drop what a phone terminal doesn't use. The flake registry alone is a
      # nixpkgs source (~200 MiB) that changes with every nixpkgs bump.
      nixpkgs.flake.setFlakeRegistry = mkDefault false;
      nixpkgs.flake.setNixPath = mkDefault false;
      environment.defaultPackages = mkDefault [];
      documentation.nixos.enable = mkDefault false;
      documentation.doc.enable = mkDefault false;
      documentation.info.enable = mkDefault false;
      # Unless Android is the default opener: the defaults and file types need it.
      xdg.mime.enable = mkDefault cfg.integrations.xdgOpen;

      # Nothing for oomd to act on (no slice is monitored), yet it polls.
      systemd.oomd.enable = mkDefault false;
      # Guest RAM is the phone's: compress before anything gets killed.
      zramSwap = {
        enable = mkDefault true;
        algorithm = mkDefault "zstd";
      };
      # Persistent (a failed boot stays readable) but small, and fewer flash
      # syncs; a config's own extraConfig replaces these.
      services.journald.extraConfig = mkDefault ''
        SystemMaxUse=256M
        SyncIntervalSec=15m
      '';
      # No display: a login on tty1 is a session nobody sees.
      systemd.services."getty@tty1".enable = false;
      systemd.services."autovt@tty1".enable = false;

      # The app's terminal speaks xterm, not serial-getty's default vt220.
      systemd.services."serial-getty@hvc0".environment.TERM = "xterm-256color";
      # The console is the recovery tab; the phone's lock screen is the gate.
      services.getty.autologinUser = mkDefault cfg.user;

      # The image ships an unregistered store; register it once, like sd-image.
      # Early boot: nix-daemon.socket is part of sockets.target, before basic.target.
      systemd.services.pocketnix-first-boot = {
        wantedBy = ["sysinit.target"];
        before = ["sysinit.target" "shutdown.target" "nix-daemon.socket" "nix-daemon.service"];
        after = ["local-fs.target"];
        conflicts = ["shutdown.target"];
        unitConfig = {
          DefaultDependencies = false;
          ConditionPathExists = "/nix-path-registration";
        };
        restartIfChanged = false;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
        };
        script = ''
          ${lib.getExe' config.nix.package.out "nix-store"} --load-db < /nix-path-registration
          touch /etc/NIXOS
          ${lib.getExe' config.nix.package.out "nix-env"} -p /nix/var/nix/profiles/system --set /run/current-system
          rm -f /nix-path-registration
        '';
      };
    })

    {
      # Tabs get no login session, so only lingering starts the user manager
      # and with it the session bus (/run/user/<uid>/bus) they and notifyd use.
      users.users.${cfg.user}.linger = mkDefault true;
    }

    (mkIf cfg.integrations.notifications {
      environment.systemPackages = [pkgs.libnotify];
      services.dbus.packages = [notifydService];
      systemd.user.services.pocketnix-notifyd = {
        description = "pocketnix notifications on Android";
        serviceConfig = {
          Type = "dbus";
          BusName = "org.freedesktop.Notifications";
          ExecStart = "${guestExe} notifyd";
        };
      };
    })

    (mkIf cfg.integrations.xdgOpen {
      environment.systemPackages = [openDesktop];
      xdg.mime.defaultApplications = lib.genAttrs openTypes (_: mkDefault "pocketnix-open.desktop");
    })
  ]);
}
