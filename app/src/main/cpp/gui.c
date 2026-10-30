// The GUI renderer's native half: zstd unpacking, copying pixels into
// AHardwareBuffers, which Java can't lock, and the mapping of guest RAM.
// Kotlin does everything else.
#include <android/hardware_buffer.h>
#include <android/hardware_buffer_jni.h>
#include <android/sharedmem.h>
#include <errno.h>
#include <jni.h>
#include <pthread.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

// From the vendored zstddeclib.c, which has no separate header.
typedef struct ZSTD_DCtx_s ZSTD_DCtx;
ZSTD_DCtx *ZSTD_createDCtx(void);
size_t ZSTD_freeDCtx(ZSTD_DCtx *dctx);
size_t ZSTD_decompressDCtx(ZSTD_DCtx *dctx, void *dst, size_t dstCapacity, const void *src, size_t srcSize);
unsigned ZSTD_isError(size_t code);
unsigned long long ZSTD_getFrameContentSize(const void *src, size_t srcSize);
size_t ZSTD_findFrameCompressedSize(const void *src, size_t srcSize);
// ZSTD_CONTENTSIZE_ERROR; ZSTD_CONTENTSIZE_UNKNOWN is the one above it.
#define CONTENTSIZE_ERROR (0ULL - 2)

// The most frames zstdFrames lists.
#define MAX_FRAMES 64

static pthread_key_t dctx_key;
static pthread_once_t dctx_once = PTHREAD_ONCE_INIT;

static void free_dctx(void *d) { ZSTD_freeDCtx(d); }
static void make_key(void) { pthread_key_create(&dctx_key, free_dctx); }

// One decompression context per thread, kept for its lifetime.
static ZSTD_DCtx *thread_dctx(void) {
	pthread_once(&dctx_once, make_key);
	ZSTD_DCtx *d = pthread_getspecific(dctx_key);
	if (!d) {
		d = ZSTD_createDCtx();
		pthread_setspecific(dctx_key, d);
	}
	return d;
}

// Decompresses src[src_off, src_off+src_len) (concatenated zstd frames) into
// dst[off, off+len). Returns the decompressed length, or -1 when the data is
// corrupt or not exactly len.
JNIEXPORT jint JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_zstdDecompress(
	JNIEnv *env, jclass cls, jbyteArray src, jint src_off, jint src_len, jbyteArray dst, jint off, jint len) {
	if (src_off < 0 || src_len < 0 || (*env)->GetArrayLength(env, src) - src_off < src_len) return -1;
	if (off < 0 || len < 0 || (*env)->GetArrayLength(env, dst) - off < len) return -1;
	ZSTD_DCtx *dctx = thread_dctx();
	if (!dctx) return -1;
	jbyte *s = (*env)->GetPrimitiveArrayCritical(env, src, NULL);
	jbyte *d = (*env)->GetPrimitiveArrayCritical(env, dst, NULL);
	size_t n = (s && d) ? ZSTD_decompressDCtx(dctx, d + off, (size_t)len, s + src_off, (size_t)src_len) : (size_t)-1;
	if (d) (*env)->ReleasePrimitiveArrayCritical(env, dst, d, 0);
	if (s) (*env)->ReleasePrimitiveArrayCritical(env, src, s, JNI_ABORT);
	if (ZSTD_isError(n) || n != (size_t)len) return -1;
	return (jint)n;
}

// Lists src's zstd frames: for each, the offset it ends at and its
// decompressed size, interleaved. NULL when src isn't whole frames that each
// state their size, or has more than MAX_FRAMES.
JNIEXPORT jlongArray JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_zstdFrames(JNIEnv *env, jclass cls, jbyteArray src) {
	jlong frames[2 * MAX_FRAMES];
	int n = 0, ok = 1;
	size_t len = (size_t)(*env)->GetArrayLength(env, src);
	jbyte *s = (*env)->GetPrimitiveArrayCritical(env, src, NULL);
	if (!s) return NULL;
	for (size_t pos = 0; pos < len; n++) {
		if (n == MAX_FRAMES) {
			ok = 0;
			break;
		}
		unsigned long long size = ZSTD_getFrameContentSize(s + pos, len - pos);
		size_t c = ZSTD_findFrameCompressedSize(s + pos, len - pos);
		if (size >= CONTENTSIZE_ERROR || ZSTD_isError(c)) {
			ok = 0;
			break;
		}
		pos += c;
		frames[2 * n] = (jlong)pos;
		frames[2 * n + 1] = (jlong)size;
	}
	(*env)->ReleasePrimitiveArrayCritical(env, src, s, JNI_ABORT);
	if (!ok) return NULL;
	jlongArray out = (*env)->NewLongArray(env, 2 * n);
	if (out) (*env)->SetLongArrayRegion(env, out, 0, 2 * n, frames);
	return out;
}

// Copies rects (x, y, w, h each; NULL for the whole buffer) of src, packed
// rows src_width * 4 bytes wide, into the locked buffer. Returns 0, or a
// negative errno-style code from the lock.
JNIEXPORT jint JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_copyRects(
	JNIEnv *env, jclass cls, jobject buffer, jbyteArray src, jint src_width, jintArray rects, jint count) {
	AHardwareBuffer *ahb = AHardwareBuffer_fromHardwareBuffer(env, buffer);
	if (!ahb || src_width <= 0) return -22;
	AHardwareBuffer_Desc desc;
	AHardwareBuffer_describe(ahb, &desc);
	jsize src_size = (*env)->GetArrayLength(env, src);
	int src_height = src_size / (src_width * 4);
	int w = (int)desc.width < src_width ? (int)desc.width : src_width;
	int h = (int)desc.height < src_height ? (int)desc.height : src_height;
	if (rects && (*env)->GetArrayLength(env, rects) < count * 4) return -22;

	jint whole[4] = {0, 0, w, h};
	jint *r = rects ? (*env)->GetPrimitiveArrayCritical(env, rects, NULL) : whole;
	if (!r) return -12;
	int n = rects ? count : 1;
	// The lock's bounds are a hint to gralloc: the union of the rects.
	ARect bounds = {w, h, 0, 0};
	for (int i = 0; i < n; i++) {
		int x0 = r[i * 4], y0 = r[i * 4 + 1], x1 = x0 + r[i * 4 + 2], y1 = y0 + r[i * 4 + 3];
		if (x0 < bounds.left) bounds.left = x0;
		if (y0 < bounds.top) bounds.top = y0;
		if (x1 > bounds.right) bounds.right = x1;
		if (y1 > bounds.bottom) bounds.bottom = y1;
	}
	if (bounds.right > w) bounds.right = w;
	if (bounds.bottom > h) bounds.bottom = h;
	if (bounds.left < 0) bounds.left = 0;
	if (bounds.top < 0) bounds.top = 0;
	if (bounds.right <= bounds.left || bounds.bottom <= bounds.top) {
		if (rects) (*env)->ReleasePrimitiveArrayCritical(env, rects, r, JNI_ABORT);
		return 0;
	}

	void *mapped = NULL;
	int err = AHardwareBuffer_lock(ahb, AHARDWAREBUFFER_USAGE_CPU_WRITE_OFTEN, -1, &bounds, &mapped);
	if (err != 0) {
		if (rects) (*env)->ReleasePrimitiveArrayCritical(env, rects, r, JNI_ABORT);
		return err < 0 ? err : -err;
	}
	jbyte *s = (*env)->GetPrimitiveArrayCritical(env, src, NULL);
	if (s) {
		size_t dst_stride = (size_t)desc.stride * 4, src_stride = (size_t)src_width * 4;
		for (int i = 0; i < n; i++) {
			int x0 = r[i * 4], y0 = r[i * 4 + 1], x1 = x0 + r[i * 4 + 2], y1 = y0 + r[i * 4 + 3];
			if (x0 < 0) x0 = 0;
			if (y0 < 0) y0 = 0;
			if (x1 > w) x1 = w;
			if (y1 > h) y1 = h;
			if (x1 <= x0) continue;
			size_t row = (size_t)(x1 - x0) * 4;
			for (int y = y0; y < y1; y++)
				memcpy((char *)mapped + y * dst_stride + (size_t)x0 * 4, s + y * src_stride + (size_t)x0 * 4, row);
		}
		(*env)->ReleasePrimitiveArrayCritical(env, src, s, JNI_ABORT);
	}
	if (rects) (*env)->ReleasePrimitiveArrayCritical(env, rects, r, JNI_ABORT);
	// A NULL fence: the unlock waits until the writes are visible to the GPU and composer.
	AHardwareBuffer_unlock(ahb, NULL);
	return s ? 0 : -12;
}

// Guest RAM, mapped read-only from the shared memory that backs it ([GuestRam]).
// The lock keeps it mapped while a copy reads from it.
#define GUEST_PAGE 4096
static pthread_rwlock_t guest_ram_lock = PTHREAD_RWLOCK_INITIALIZER;
static const uint8_t *guest_ram;
static size_t guest_ram_len;
static uint64_t guest_ram_base;

// Makes and maps shared memory of `size` bytes standing for guest-physical
// [base, base + size), replacing any mapped before. Returns its fd, or -errno.
JNIEXPORT jint JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_guestRamCreate(JNIEnv *env, jclass cls, jlong base, jlong size) {
	int fd = ASharedMemory_create("pocketnix-guest-ram", (size_t)size);
	if (fd < 0) return -errno;
	void *m = mmap(NULL, (size_t)size, PROT_READ, MAP_SHARED, fd, 0);
	if (m == MAP_FAILED) {
		int e = errno;
		close(fd);
		return -e;
	}
	pthread_rwlock_wrlock(&guest_ram_lock);
	if (guest_ram) munmap((void *)guest_ram, guest_ram_len);
	guest_ram = m;
	guest_ram_len = (size_t)size;
	guest_ram_base = (uint64_t)base;
	pthread_rwlock_unlock(&guest_ram_lock);
	return fd;
}

JNIEXPORT void JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_guestRamRelease(JNIEnv *env, jclass cls) {
	pthread_rwlock_wrlock(&guest_ram_lock);
	if (guest_ram) munmap((void *)guest_ram, guest_ram_len);
	guest_ram = NULL;
	guest_ram_len = 0;
	pthread_rwlock_unlock(&guest_ram_lock);
}

static uint64_t be64(const uint8_t *p) {
	uint64_t v = 0;
	for (int i = 0; i < 8; i++) v = v << 8 | p[i];
	return v;
}

// Copies the first len bytes of the guest pages that src lists (gui-protocol.md's
// PAGES) into dst. Returns 0; -22 for a list that doesn't cover exactly len
// bytes inside guest RAM, -19 with no guest RAM mapped.
JNIEXPORT jint JNICALL Java_dev_nickolaj_pocketnix_gui_GuiNative_guestRamGather(JNIEnv *env, jclass cls, jbyteArray src, jbyteArray dst, jint len) {
	size_t n = (size_t)(*env)->GetArrayLength(env, src), pages = ((size_t)len + GUEST_PAGE - 1) / GUEST_PAGE;
	if (len < 0 || n != 4 + 8 * pages || (size_t)(*env)->GetArrayLength(env, dst) < (size_t)len) return -22;
	pthread_rwlock_rdlock(&guest_ram_lock);
	int err = guest_ram ? 0 : -19;
	uint8_t *s = err ? NULL : (*env)->GetPrimitiveArrayCritical(env, src, NULL);
	uint8_t *d = err ? NULL : (*env)->GetPrimitiveArrayCritical(env, dst, NULL);
	if (!err && (!s || !d)) err = -12;
	if (!err && ((uint32_t)s[0] << 24 | (uint32_t)s[1] << 16 | (uint32_t)s[2] << 8 | s[3]) != pages) err = -22;
	for (size_t i = 0; !err && i < pages; i++) {
		// Unsigned: an address below the base wraps past the end.
		uint64_t at = be64(s + 4 + 8 * i) - guest_ram_base;
		size_t chunk = (size_t)len - i * GUEST_PAGE < GUEST_PAGE ? (size_t)len - i * GUEST_PAGE : GUEST_PAGE;
		if (at > guest_ram_len || guest_ram_len - at < chunk) {
			err = -22;
			break;
		}
		memcpy(d + i * GUEST_PAGE, guest_ram + at, chunk);
	}
	if (d) (*env)->ReleasePrimitiveArrayCritical(env, dst, d, 0);
	if (s) (*env)->ReleasePrimitiveArrayCritical(env, src, s, JNI_ABORT);
	pthread_rwlock_unlock(&guest_ram_lock);
	return err;
}
