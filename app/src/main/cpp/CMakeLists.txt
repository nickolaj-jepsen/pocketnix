cmake_minimum_required(VERSION 3.22)
project(pocketnix_gui C)

# zstd/zstddeclib.c: zstd 1.5.7's build/single_file_libs/create_single_file_decoder.sh output, unmodified.
add_library(pocketnix_gui SHARED gui.c zstd/zstddeclib.c)
# Debug builds compile at -O0, which makes zstd several times slower; the later -O2 wins.
target_compile_options(pocketnix_gui PRIVATE -O2 -fvisibility=hidden)
set_source_files_properties(gui.c PROPERTIES COMPILE_OPTIONS "-Wall;-Werror")
# Keep zstd's API out of the library's exports; only the JNI entry points are public.
target_compile_definitions(pocketnix_gui PRIVATE ZSTDLIB_VISIBLE=)
target_link_libraries(pocketnix_gui android nativewindow)
