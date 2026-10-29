{pkgs, ...}: {
  pocketnix.guest = {
    enable = true;
    user = "alice"; # terminal tabs and the console log in as this user
    forwards.ssh = {
      guestPort = 22;
      hostPort = 2022;
    };
  };

  users.users.alice = {
    isNormalUser = true;
    extraGroups = ["wheel"];
  };
  security.sudo.wheelNeedsPassword = false;
  # Keys arrive with `pocketnix provision --inject /etc/ssh/authorized_keys.d/alice=<key.pub> --mode 0644`.
  services.openssh = {
    enable = true;
    settings.PasswordAuthentication = false;
  };
  environment.systemPackages = [pkgs.htop];

  networking.hostName = "phone";
  system.stateVersion = "26.05";
}
