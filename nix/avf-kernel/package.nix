# Google's prebuilt guest kernel for the Android Terminal's (AVF) VM: under
# Pixel pKVM, nixpkgs' kernels read corrupted virtio disk data; this one reads
# clean. The Image plus its initramfs's module tree (most drivers are built in).
# NixOS's boot.kernelPackages re-overrides every kernel with kernelPatches,
# features and randstructSeed; they come in through `...` because callPackage
# would fill a named `kernelPatches` with pkgs.kernelPatches.
{
  lib,
  fetchurl,
  fetchgit,
  cpio,
  ...
} @ args: let
  version = "6.12.93";
  modDirVersion = "6.12.93-android16-6-g3a7d1771d492-ab16496487-4k";
  # The commit in modDirVersion (g…), for out-of-tree modules; refresh it with the Image.
  sourceRev = "3a7d1771d4925a56f7eeb8a5ba1faff0c544a9ef";

  # The Image's embedded .config (IKCFG), vendored because NixOS modules query
  # it at eval time; refresh it together with the hash.
  options = lib.listToAttrs (lib.concatMap (line: let
    m = builtins.match "(CONFIG_[A-Za-z0-9_]+)=(.*)" line;
  in
    lib.optional (m != null) (lib.nameValuePair (lib.elemAt m 0) (lib.elemAt m 1)))
  (lib.splitString "\n" (builtins.readFile ./config)));
  # Same interface as nixpkgs-built kernels' `config`.
  config = let
    getValue = attr: options."CONFIG_${attr}" or null;
  in
    options
    // rec {
      isSet = attr: options ? "CONFIG_${attr}";
      inherit getValue;
      isYes = attr: getValue attr == "y";
      isNo = attr: getValue attr == "n";
      isModule = attr: getValue attr == "m";
      isEnabled = attr: isModule attr || isYes attr;
      isDisabled = attr: !(isSet attr) || isNo attr;
    };
in
  assert lib.assertMsg ((args.kernelPatches or []) == []) "avf-kernel is prebuilt; boot.kernelPatches can't apply to it";
  assert lib.assertMsg (lib.hasInfix "-g${builtins.substring 0 12 sourceRev}-" modDirVersion) "avf-kernel: sourceRev isn't the commit modDirVersion names";
    fetchurl {
      pname = "avf-kernel";
      inherit version;
      # Unversioned: Google replaces it with each Terminal image build (see build_id),
      # so a hash mismatch means a new kernel to vet, not a flaky download.
      url = "https://dl.google.com/android/ferrochrome/5100000/aarch64/images.tar.gz";
      downloadToTemp = true;
      recursiveHash = true;
      nativeBuildInputs = [cpio];
      postFetch = ''
        mkdir -p $out
        tar -xzf $downloadedFile -C $out vmlinuz build_id
        mv $out/vmlinuz $out/Image
        tar -xzf $downloadedFile -O initrd.img | gzip -d | (cd $out && cpio -id --quiet 'lib/modules/*')
      '';
      hash = "sha256-d1x02NfUeqYGalKR8BF6id3tcIQEX6EzjmPi1mCjZaM=";

      passthru = {
        # A `features` attr makes NixOS skip its kernel-config assertions, which
        # need a parsed .config this prebuilt doesn't carry.
        inherit version modDirVersion config;
        configfile = ./config;
        # The tree the Image was built from (nix/vgpu-relinquish builds against it).
        kernelSource = fetchgit {
          url = "https://android.googlesource.com/kernel/common";
          rev = sourceRev;
          hash = "sha256-rGJ0nu1kEcB4xLAwq6UlWxW45gzcUbnwT5CtfXUD6yc=";
        };
        # CONFIG_EFI_STUB=y; systemd-boot asserts on this flag.
        features = {efiBootStub = true;} // args.features or {};
        kernelOlder = lib.versionOlder version;
        kernelAtLeast = lib.versionAtLeast version;
        isHardened = false;
        isLibre = false;
        isZen = false;
      };

      meta = {
        description = "Prebuilt android16-6.12 kernel for AVF guests on Pixel phones";
        homepage = "https://source.android.com/docs/core/virtualization";
        license = lib.licenses.gpl2Only;
        platforms = ["aarch64-linux"];
        sourceProvenance = [lib.sourceTypes.binaryNativeCode];
      };
    }
