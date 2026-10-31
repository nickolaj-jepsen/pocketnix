# vgpu_relinquish.ko for nix/avf-kernel, which ships no headers or
# Module.symvers: built against the tree the Image came from, with its
# vendored config, and the CRCs its own modules recorded (crcs.py).
{
  lib,
  stdenv,
  kernel,
  llvmPackages_19,
  bc,
  bison,
  flex,
  perl,
  python3,
  openssl,
  elfutils,
  zlib,
}: let
  llvm = llvmPackages_19;
  make = lib.escapeShellArgs [
    "ARCH=arm64"
    "LLVM=1"
    # The wrapped clang can't target the kernel; host tools need the wrapper's libc.
    "CC=${llvm.clang-unwrapped}/bin/clang"
    "HOSTCC=${llvm.clang}/bin/clang"
    # The exact release, so vermagic matches without leaning on CRCs.
    "LOCALVERSION=${lib.removePrefix kernel.version kernel.modDirVersion}"
  ];
in
  stdenv.mkDerivation {
    pname = "vgpu-relinquish";
    inherit (kernel) version;
    src = lib.fileset.toSource {
      root = ./.;
      fileset = lib.fileset.unions [./vgpu_relinquish.c ./Kbuild];
    };

    nativeBuildInputs = [llvm.clang llvm.lld llvm.llvm bc bison flex perl python3];
    buildInputs = [openssl elfutils zlib];
    hardeningDisable = ["all"];
    # A kernel module has no interpreter or RPATH to patch.
    dontPatchELF = true;

    buildPhase = ''
      runHook preBuild
      # The tree as symlinks, but scripts/ copied: kbuild runs its #!/usr/bin/env scripts directly.
      tree=$NIX_BUILD_TOP/linux build=$NIX_BUILD_TOP/build
      cp -rs ${kernel.kernelSource} $tree
      chmod -R u+w $tree
      rm -r $tree/scripts
      cp -r ${kernel.kernelSource}/scripts $tree/scripts
      chmod -R u+w $tree/scripts
      patchShebangs --build $tree/scripts

      mkdir $build
      install -m644 ${kernel.configfile} $build/.config
      # Rust, BTF and the 32-bit vDSO need toolchains this doesn't carry; none
      # changes vermagic or the structs the module reads. LOCALVERSION carries
      # the whole suffix.
      $tree/scripts/config --file $build/.config \
        -d RUST -d DEBUG_INFO_BTF -d DEBUG_INFO_BTF_MODULES -d COMPAT_VDSO \
        -d MODULE_SIG_ALL -d LOCALVERSION_AUTO --set-str LOCALVERSION ""
      make -C $tree O=$build -j$NIX_BUILD_CORES ${make} olddefconfig modules_prepare
      release=$(cat $build/include/config/kernel.release)
      [ "$release" = ${lib.escapeShellArg kernel.modDirVersion} ] || { echo "built for $release, the Image is ${kernel.modDirVersion}" >&2; exit 1; }

      python3 -I ${./crcs.py} $build/known.symvers $(find ${kernel}/lib/modules -name '*.ko')
      make -C $build -j$NIX_BUILD_CORES ${make} M=$PWD KBUILD_EXTRA_SYMBOLS=$build/known.symvers KBUILD_MODPOST_WARN=1 modules
      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      install -Dm444 vgpu_relinquish.ko $out/lib/modules/${kernel.modDirVersion}/extra/vgpu_relinquish.ko
      runHook postInstall
    '';

    meta = {
      description = "Lets the guest map virtio-gpu blobs more than once under Pixel pKVM";
      license = lib.licenses.gpl2Only;
      platforms = ["aarch64-linux"];
    };
  }
