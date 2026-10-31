# A minimal pocketnix guest, which the tests boot too.
# Build its image with `nix build .#example-image`.
{pkgs, ...}: {
  pocketnix.guest = {
    enable = true;
    user = "nixos";
    # Forwards share the phone's ports with other apps; 2222 is a popular one.
    forwards.ssh = {
      guestPort = 22;
      hostPort = 2022;
    };
    terminal = {
      profiles = {
        shell = {};
        htop.command = "htop";
      };
      theme.colors = import ../nix/catppuccin-mocha.nix;
      theme.fontFamily = {
        package = pkgs.jetbrains-mono;
        family = "JetBrains Mono";
      };
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
