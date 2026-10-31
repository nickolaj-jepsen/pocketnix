"""crcs.py <out.symvers> <module.ko>...: a Module.symvers holding the CRC of every
symbol the given modules import, as recorded in their __versions sections.

The prebuilt kernel ships no Module.symvers (and Android CI's vmlinux.symvers for
its build id holds other CRCs), but its own modules carry the CRCs it was built
with. A symbol none of them imports is left out, and the kernel loads a module
whose __versions lacks it ("no symbol version").
"""

import struct
import sys

ENTRY = 64  # struct modversion_info: unsigned long crc; char name[56]


def versions(path):
    d = open(path, 'rb').read()
    shoff = struct.unpack_from('<Q', d, 0x28)[0]
    shentsize, shnum, shstrndx = struct.unpack_from('<HHH', d, 0x3a)
    secs = [struct.unpack_from('<IIQQQQIIQQ', d, shoff + i * shentsize) for i in range(shnum)]
    names = secs[shstrndx][4]
    for name_off, _, _, _, off, size, *_ in secs:
        if d[names + name_off:].split(b'\0', 1)[0] != b'__versions':
            continue
        for e in range(off, off + size, ENTRY):
            yield d[e + 8:e + ENTRY].split(b'\0', 1)[0].decode(), struct.unpack_from('<Q', d, e)[0]


crcs = {}
for path in sys.argv[2:]:
    for name, crc in versions(path):
        if crcs.setdefault(name, crc) != crc:
            sys.exit(f'{path}: {name} has CRC {crc:#x}, another module {crcs[name]:#x}')
with open(sys.argv[1], 'w') as f:
    for name, crc in sorted(crcs.items()):
        f.write(f'0x{crc:08x}\t{name}\tvmlinux\tEXPORT_SYMBOL\t\n')
