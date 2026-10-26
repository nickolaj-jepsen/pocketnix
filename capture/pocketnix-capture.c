// pocketnix-capture: one long-lived wlr-screencopy client, for the agent's
// display stream. Prints, per frame, the damage rects and their pixels as RGBA,
// so the agent neither spawns a process per frame nor round-trips through PPM.
//
// Wire format on stdout (big-endian):
//
//	[u32 width][u32 height][u32 rects]
//	rects × [u32 x][u32 y][u32 w][u32 h]
//	then, per rect, h rows of w × 4 bytes of RGBA
//
// A frame with no damage is not printed at all. Rects are clipped to the output
// and split so a rect's pixels always fit the agent's 1 MiB frame cap.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

#include <wayland-client.h>
#include "wlr-screencopy.h"

#define MAX_DAMAGE 64
// The agent's protocol.MaxFramePayload minus the band frame's header.
#define MAX_BAND_BYTES (1048576 - 8)

struct rect { int32_t x, y, w, h; };

struct state {
    struct wl_display *dpy;
    struct wl_registry *registry;
    struct wl_shm *shm;
    struct wl_output *output;
    struct zwlr_screencopy_manager_v1 *manager;

    // One capture's worth of state, reset before every frame.
    int ready, failed, have_format;
    uint32_t format, width, height, stride;
    uint32_t shm_format;
    struct wl_buffer *buffer;
    void *pixels;
    size_t size;
    int fd;
    struct rect damage[MAX_DAMAGE];
    int ndamage;
    int emitted;
};

static void die(const char *what) {
    fprintf(stderr, "pocketnix-capture: %s: %s\n", what, strerror(errno));
    exit(1);
}

static int create_shm_file(size_t size) {
    int fd = memfd_create("pocketnix-capture", MFD_CLOEXEC);
    if (fd < 0) die("memfd_create");
    if (ftruncate(fd, (off_t)size) < 0) die("ftruncate");
    return fd;
}

static void frame_buffer(void *data, struct zwlr_screencopy_frame_v1 *frame,
                         uint32_t format, uint32_t width, uint32_t height, uint32_t stride) {
    struct state *s = data;
    s->format = format;
    s->width = width;
    s->height = height;
    s->stride = stride;
    s->have_format = 1;
    // The buffer must exist before copy, so it is made here and handed over.
    s->size = (size_t)stride * height;
    s->fd = create_shm_file(s->size);
    s->pixels = mmap(NULL, s->size, PROT_READ | PROT_WRITE, MAP_SHARED, s->fd, 0);
    if (s->pixels == MAP_FAILED) die("mmap");
    struct wl_shm_pool *pool = wl_shm_create_pool(s->shm, s->fd, (int32_t)s->size);
    s->buffer = wl_shm_pool_create_buffer(pool, 0, (int32_t)width, (int32_t)height, (int32_t)stride, format);
    wl_shm_pool_destroy(pool);
    // copy_with_damage is what makes the compositor send damage events.
    zwlr_screencopy_frame_v1_copy_with_damage(frame, s->buffer);
}

static void frame_flags(void *data, struct zwlr_screencopy_frame_v1 *frame, uint32_t flags) {
    (void)data;
    (void)frame;
    (void)flags;
}

static void frame_damage(void *data, struct zwlr_screencopy_frame_v1 *frame,
                         uint32_t x, uint32_t y, uint32_t w, uint32_t h) {
    (void)frame;
    struct state *s = data;
    if (w && h && s->ndamage < MAX_DAMAGE) {
        s->damage[s->ndamage++] = (struct rect){(int32_t)x, (int32_t)y, (int32_t)w, (int32_t)h};
    }
}

static void frame_linux_dmabuf(void *data, struct zwlr_screencopy_frame_v1 *frame,
                               uint32_t format, uint32_t width, uint32_t height) {
    (void)data;
    (void)frame;
    (void)format;
    (void)width;
    (void)height;
    // Shm only: the guest has no GPU path to hand out dmabufs anyway.
}

static void frame_ready(void *data, struct zwlr_screencopy_frame_v1 *frame,
                        uint32_t sec_hi, uint32_t sec_lo, uint32_t nsec) {
    (void)frame;
    (void)sec_hi;
    (void)sec_lo;
    (void)nsec;
    ((struct state *)data)->ready = 1;
}

static void frame_failed(void *data, struct zwlr_screencopy_frame_v1 *frame) {
    (void)frame;
    ((struct state *)data)->failed = 1;
}

static void frame_buffer_done(void *data, struct zwlr_screencopy_frame_v1 *frame) {
    (void)data;
    (void)frame;
}

static const struct zwlr_screencopy_frame_v1_listener frame_listener = {
    .buffer = frame_buffer,
    .flags = frame_flags,
    .damage = frame_damage,
    .linux_dmabuf = frame_linux_dmabuf,
    .ready = frame_ready,
    .failed = frame_failed,
    .buffer_done = frame_buffer_done,
};

static void put_u32(uint32_t v) {
    uint8_t b[4] = {v >> 24, v >> 16, v >> 8, v};
    fwrite(b, 1, 4, stdout);
}

// The damage rects, clipped to the output and split so each fits one agent
// frame. Overlaps are left alone: sending a row twice is harmless.
static int rects_from_damage(struct state *s, struct rect *out, int max) {
    int n = 0;
    for (int i = 0; i < s->ndamage && n < max; i++) {
        int32_t x0 = s->damage[i].x < 0 ? 0 : s->damage[i].x;
        int32_t y0 = s->damage[i].y < 0 ? 0 : s->damage[i].y;
        int32_t x1 = s->damage[i].x + s->damage[i].w;
        int32_t y1 = s->damage[i].y + s->damage[i].h;
        if (x1 > (int32_t)s->width) x1 = (int32_t)s->width;
        if (y1 > (int32_t)s->height) y1 = (int32_t)s->height;
        if (x1 <= x0 || y1 <= y0) continue;
        uint32_t w = (uint32_t)(x1 - x0);
        uint32_t maxrows = MAX_BAND_BYTES / (w * 4);
        if (maxrows == 0) maxrows = 1;
        for (int32_t y = y0; y < y1 && n < max; y += (int32_t)maxrows) {
            uint32_t h = (uint32_t)(y1 - y);
            if (h > maxrows) h = maxrows;
            out[n++] = (struct rect){x0, y, (int32_t)w, (int32_t)h};
        }
    }
    if (n == 0 && s->ndamage == 0 && !s->emitted) {
        // The first copy reports the whole output; be explicit about it.
        uint32_t maxrows = MAX_BAND_BYTES / (s->width * 4);
        if (maxrows == 0) maxrows = 1;
        for (int32_t y = 0; y < (int32_t)s->height && n < max; y += (int32_t)maxrows) {
            uint32_t h = (uint32_t)((int32_t)s->height - y);
            if (h > maxrows) h = maxrows;
            out[n++] = (struct rect){0, y, (int32_t)s->width, (int32_t)h};
        }
    }
    return n;
}

// Emit one frame: the rects, then their pixels as RGBA.
static void emit_frame(struct state *s) {
    struct rect rects[256];
    int n = rects_from_damage(s, rects, 256);
    if (n == 0) return;
    s->emitted = 1;
    put_u32(s->width);
    put_u32(s->height);
    put_u32((uint32_t)n);
    for (int i = 0; i < n; i++) {
        put_u32((uint32_t)rects[i].x);
        put_u32((uint32_t)rects[i].y);
        put_u32((uint32_t)rects[i].w);
        put_u32((uint32_t)rects[i].h);
    }
    uint8_t *rgba = malloc(MAX_BAND_BYTES);
    if (!rgba) die("malloc");
    for (int i = 0; i < n; i++) {
        uint32_t w = (uint32_t)rects[i].w, rows = (uint32_t)rects[i].h;
        size_t bytes = (size_t)w * 4 * rows;
        for (uint32_t r = 0; r < rows; r++) {
            const uint8_t *src = (const uint8_t *)s->pixels +
                                 (size_t)(rects[i].y + (int32_t)r) * s->stride +
                                 (size_t)rects[i].x * 4;
            uint8_t *dst = rgba + (size_t)r * w * 4;
            for (uint32_t p = 0; p < w * 4; p += 4) {
                // Shm is ARGB8888 (B,G,R,X on little-endian); the app takes RGBA.
                dst[p] = src[p + 2];
                dst[p + 1] = src[p + 1];
                dst[p + 2] = src[p];
                dst[p + 3] = 0xff;
            }
        }
        fwrite(rgba, 1, bytes, stdout);
    }
    free(rgba);
}

static void registry_global(void *data, struct wl_registry *registry, uint32_t name,
                            const char *interface, uint32_t version) {
    struct state *s = data;
    if (strcmp(interface, wl_shm_interface.name) == 0) {
        s->shm = wl_registry_bind(registry, name, &wl_shm_interface, 1);
    } else if (strcmp(interface, wl_output_interface.name) == 0 && !s->output) {
        s->output = wl_registry_bind(registry, name, &wl_output_interface, version < 2 ? version : 2);
    } else if (strcmp(interface, zwlr_screencopy_manager_v1_interface.name) == 0) {
        s->manager = wl_registry_bind(registry, name, &zwlr_screencopy_manager_v1_interface, 3);
    }
}

static void registry_remove(void *data, struct wl_registry *registry, uint32_t name) {
    (void)data;
    (void)registry;
    (void)name;
}

static const struct wl_registry_listener registry_listener = {registry_global, registry_remove};

int main(int argc, char **argv) {
    long min_frame_ms = 8;
    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--min-frame-ms") == 0 && i + 1 < argc) min_frame_ms = strtol(argv[++i], NULL, 10);
    }
    static struct state s;
    s.dpy = wl_display_connect(NULL);
    if (!s.dpy) die("wl_display_connect");
    s.registry = wl_display_get_registry(s.dpy);
    wl_registry_add_listener(s.registry, &registry_listener, &s);
    wl_display_roundtrip(s.dpy);
    if (!s.shm) die("compositor has no wl_shm");
    if (!s.output) die("compositor has no wl_output");
    if (!s.manager) die("compositor has no zwlr_screencopy_manager_v1");

    for (;;) {
        struct timespec t0, t1;
        clock_gettime(CLOCK_MONOTONIC, &t0);
        s.ready = s.failed = s.have_format = 0;
        s.ndamage = 0;
        s.buffer = NULL;
        s.pixels = NULL;
        struct zwlr_screencopy_frame_v1 *frame =
            zwlr_screencopy_manager_v1_capture_output(s.manager, 0, s.output);
        zwlr_screencopy_frame_v1_add_listener(frame, &frame_listener, &s);
        while (!s.ready && !s.failed) {
            if (wl_display_dispatch(s.dpy) < 0) die("wl_display_dispatch");
        }
        if (s.failed) {
            fprintf(stderr, "pocketnix-capture: capture failed\n");
        } else {
            emit_frame(&s);
            fflush(stdout);
        }
        if (s.buffer) wl_buffer_destroy(s.buffer);
        if (s.pixels) munmap(s.pixels, s.size);
        if (s.have_format) close(s.fd);
        zwlr_screencopy_frame_v1_destroy(frame);
        clock_gettime(CLOCK_MONOTONIC, &t1);
        long elapsed = (t1.tv_sec - t0.tv_sec) * 1000 + (t1.tv_nsec - t0.tv_nsec) / 1000000;
        if (elapsed < min_frame_ms) {
            struct timespec rest = {(min_frame_ms - elapsed) / 1000, ((min_frame_ms - elapsed) % 1000) * 1000000};
            nanosleep(&rest, NULL);
        }
    }
}
