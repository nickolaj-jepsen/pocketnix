# A minimal pocketnix guest: the test image and the NixOS test boot this.
# Build its image with `nix build .#example-image`.
{pkgs, ...}: {
  pocketnix.guest = {
    enable = true;
    user = "nixos";
    # Not 2222: forwards share the phone's ports with other apps' (Podroid's ssh takes 2222).
    forwards.ssh = {
      guestPort = 22;
      hostPort = 2022;
    };
    terminal = {
      profiles = {
        shell = {};
        htop.command = "htop";
      };
      # Catppuccin Mocha.
      theme.colors = {
        background = "#1e1e2e";
        foreground = "#cdd6f4";
        cursor = "#f5e0dc";
        color0 = "#45475a";
        color1 = "#f38ba8";
        color2 = "#a6e3a1";
        color3 = "#f9e2af";
        color4 = "#89b4fa";
        color5 = "#f5c2e7";
        color6 = "#94e2d5";
        color7 = "#bac2de";
        color8 = "#585b70";
        color9 = "#f38ba8";
        color10 = "#a6e3a1";
        color11 = "#f9e2af";
        color12 = "#89b4fa";
        color13 = "#f5c2e7";
        color14 = "#94e2d5";
        color15 = "#a6adc8";
      };
      theme.font = "${pkgs.jetbrains-mono}/share/fonts/truetype/JetBrainsMono-Regular.ttf";
      snippets = {
        "Follow the journal" = "journalctl -f";
        "Disk usage" = {
          command = "df -h /";
          submit = true;
        };
      };
    };
  };

  networking.hostName = "pocketnix-example";
  users.users.nixos = {
    isNormalUser = true;
    extraGroups = ["wheel"];
  };
  security.sudo.wheelNeedsPassword = false;
  # Keys arrive with `pocketnix provision --inject /etc/ssh/authorized_keys.d/nixos=<key.pub> --mode 0644`.
  services.openssh = {
    enable = true;
    settings.PasswordAuthentication = false;
  };
  environment.systemPackages = [pkgs.htop];

  system.stateVersion = "26.05";
}
