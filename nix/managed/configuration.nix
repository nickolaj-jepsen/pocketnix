# Your additions to this guest's NixOS configuration. The app's System menu
# rebuilds it; packages you add in the app go to your user profile instead.
{pkgs, ...}: {
  # environment.systemPackages = [pkgs.htop];
  system.stateVersion = "26.05";
}
