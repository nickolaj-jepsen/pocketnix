# What Gradle needs to build the app: the devshell's and `nix run .#gradle`'s.
# pkgs needs allowUnfree and android_sdk.accept_license.
{pkgs}: let
  buildToolsVersion = "36.0.0";
  sdk =
    (pkgs.androidenv.composeAndroidPackages {
      # The app compiles against 36.1, the terminal modules against 36.
      platformVersions = ["36" "36.1"];
      buildToolsVersions = [buildToolsVersion];
      includeNDK = true;
      # AGP's default NDK; it tries to download any other into the read-only SDK.
      ndkVersions = ["28.2.13676358"];
    }).androidsdk;
  sdkRoot = "${sdk}/libexec/android-sdk";
in {
  packages = [pkgs.jdk21 pkgs.android-tools];
  env = {
    ANDROID_HOME = sdkRoot;
    ANDROID_SDK_ROOT = sdkRoot;
    JAVA_HOME = pkgs.jdk21.home;
    # AGP fetches a dynamically linked aapt2 from Maven, which can't run on NixOS.
    GRADLE_OPTS = "-Dorg.gradle.project.android.aapt2FromMavenOverride=${sdkRoot}/build-tools/${buildToolsVersion}/aapt2";
  };
}
