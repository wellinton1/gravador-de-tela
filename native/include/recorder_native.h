/*
 * recorder_native.h - stable C ABI for the recorder's native layer.
 *
 * Everything low level lives behind this boundary: adapter discovery, DXGI
 * desktop duplication with a GDI fallback, WIC Motion JPEG muxed into AVI
 * and WASAPI loopback audio. The Go side owns the user interface and the
 * recording logic, and reaches this library through the plain syscall package,
 * so the ABI has to be plain C with fixed-width types and no ownership rules
 * hidden behind C++ types.
 *
 * Rules for this header:
 *   - every struct starts with a size field so the caller can detect a mismatch;
 *   - strings are UTF-8, fixed size, NUL terminated;
 *   - a function that can fail returns a RecorderStatus and never throws;
 *   - the native library never keeps a pointer the caller gave it.
 */
#ifndef RECORDER_NATIVE_H
#define RECORDER_NATIVE_H

#include <stdint.h>

/* Every entry point is exported with dllexport rather than through a module
 * definition file, because the project lives in a path that contains a space and
 * a .def file passed to the linker would have to be quoted to survive it. */
#if defined(RECORDER_BUILDING_DLL)
#define RECORDER_API __declspec(dllexport)
#else
#define RECORDER_API
#endif
#ifdef __cplusplus
extern "C" {
#endif

/* Bumped whenever the layout of any struct or the meaning of any status changes. */
#define RECORDER_NATIVE_ABI 3

/* Size of the UTF-8 description buffers in RecorderAdapterInfo. */
#define RECORDER_ADAPTER_DESC_CAP 128
#define RECORDER_ADAPTER_NAME_CAP 32

typedef enum RecorderStatus {
    RecorderStatusOk = 0,
    /* A real failure; the message is available from the matching last_error. */
    RecorderStatusError = 1,
    /* No new frame arrived in time. Not a failure: a static desktop legitimately
     * produces no updates, so the caller should simply ask again. */
    RecorderStatusTimeout = 2,
    /* The machine cannot do what was asked, for example no encoder exists for the
     * requested codec. The caller is expected to fall back, not to give up. */
    RecorderStatusUnsupported = 3,
    /* A device or output disappeared. The caller should re-enumerate. */
    RecorderStatusLost = 4
} RecorderStatus;

typedef enum RecorderBackend {
    RecorderBackendNone = 0,
    /* DXGI desktop duplication: fast, includes the hardware cursor. */
    RecorderBackendDxgi = 1,
    /* GDI BitBlt: slower, works where duplication is refused. */
    RecorderBackendGdi = 2
} RecorderBackend;

typedef enum RecorderRotation {
    RecorderRotationUnspecified = 0,
    RecorderRotation90 = 1,
    RecorderRotation180 = 2,
    RecorderRotation270 = 3
} RecorderRotation;

/* Panel rotation in quarter-turns from upright, translated from
 * DXGI_MODE_ROTATION so Go does not have to know DXGI. */
typedef enum RecorderRotationDxgi {
    RecorderDxgiRotationUnspecified = 0,
    RecorderDxgiRotation90 = 1,
    RecorderDxgiRotation180 = 2,
    RecorderDxgiRotation270 = 3
} RecorderRotationDxgi;

typedef struct RecorderRect {
    int32_t left;
    int32_t top;
    int32_t right;
    int32_t bottom;
} RecorderRect;

/*
 * One display output. A machine with several adapters produces one entry per
 * output, and every field the recorder needs to describe it to a user is filled
 * in so the interface never has to guess.
 */
typedef struct RecorderAdapterInfo {
    uint32_t struct_size;
    uint32_t adapter_index; /* index into the DXGI adapter list */
    uint32_t output_index;  /* index into that adapter's output list */
    RecorderRect bounds;    /* position and size in virtual desktop coordinates */
    uint32_t rotation;      /* RecorderRotationDxgi */
    float refresh_hz;
    uint32_t attached_to_desktop;
    uint32_t primary;
    uint32_t can_duplicate; /* duplication was accepted for this output */
    uint32_t is_software;   /* WARP or a basic display adapter, not a real GPU */
    uint32_t vendor_id;
    uint32_t device_id;
    char description[RECORDER_ADAPTER_DESC_CAP];
    char device_name[RECORDER_ADAPTER_NAME_CAP];
} RecorderAdapterInfo;

/*
 * What the machine can actually do. This is the answer to "does this computer
 * have a video card worth using", and it is reported rather than assumed so the
 * interface can explain a software fallback instead of silently degrading.
 */
typedef struct RecorderCapabilities {
    uint32_t struct_size;
    uint32_t adapter_count;         /* outputs found */
    uint32_t adapter_count_hardware;/* outputs backed by real hardware */
    uint32_t has_dxgi_duplication;   /* at least one output can be duplicated */
    uint32_t hardware_h264;
    uint32_t hardware_h265;
    uint32_t software_h264;
    uint32_t software_h265;
    uint32_t audio_loopback;         /* a render endpoint can be captured */
    char renderer[RECORDER_ADAPTER_DESC_CAP]; /* adapter the encoder runs on */
    /* ABI 2 additions. Everything above keeps its v1 offset. */
    uint32_t mjpeg;                  /* WIC JPEG encoder present */
} RecorderCapabilities;

/* ---------------------------------------------------------------- capture - */

typedef struct RecorderCaptureConfig {
    uint32_t struct_size;
    uint32_t adapter_index;
    uint32_t output_index;
    uint32_t flags;
    /* Region in virtual desktop coordinates. When has_region is zero the whole
     * output is captured. */
    RecorderRect region;
    uint32_t has_region;
    uint32_t track_cursor;       /* report the pointer rectangle */
    uint32_t draw_cursor;        /* draw the pointer into the pixels (GDI only) */
    uint32_t hide_cursor_on_click;
    uint32_t prefer_gdi;         /* skip DXGI and go straight to the fallback */
    uint32_t reserved;
} RecorderCaptureConfig;

enum {
    RecorderCaptureFlagNone = 0,
    /* Draw a red ring where the pointer is while its button is held. */
    RecorderCaptureFlagClickRing = 1,
    /* Draw a soft halo around the pointer, always. */
    RecorderCaptureFlagHalo = 2
};

typedef struct RecorderCursor {
    uint32_t valid;
    int32_t left;
    int32_t top;
    int32_t right;
    int32_t bottom;
} RecorderCursor;

/*
 * A capture session. Opaque: the caller only ever holds the pointer returned by
 * recorder_capture_open and hands it back to the other calls.
 */
typedef struct RecorderCapture RecorderCapture;

/* Frame pixels are BGRA8, top-down, and stay valid until the next grab. */
typedef struct RecorderFrame {
    uint32_t struct_size;
    uint32_t width;
    uint32_t height;
    int32_t stride;        /* bytes per row, may exceed width * 4 */
    uint64_t present_ticks;/* 100ns units since boot, 0 when not reported */
    uint32_t accumulated_frames; /* updates folded into this frame */
    RecorderCursor cursor;
} RecorderFrame;

/* ---------------------------------------------------------------- encoder - */

typedef enum RecorderCodec {
    /* Reserved for a future Media Foundation path; this build only encodes MJPEG. */
    RecorderCodecH264 = 0,
    RecorderCodecH265 = 1,
    /* Motion JPEG: one WIC JPEG per frame, muxed into AVI. Always available. */
    RecorderCodecMjpeg = 2
} RecorderCodec;

typedef enum RecorderContainer {
    RecorderContainerMp4 = 0,
    RecorderContainerAvi = 1
} RecorderContainer;

typedef struct RecorderEncoderConfig {
    uint32_t struct_size;
    uint32_t width;
    uint32_t height;
    uint32_t frames_per_second;
    uint32_t bits_per_second; /* ignored by MJPEG; kept so H.264 settings survive */
    uint32_t codec;        /* RecorderCodec; only RecorderCodecMjpeg is supported */
    uint32_t container;    /* RecorderContainer; only RecorderContainerAvi is supported */
    uint32_t allow_hardware; /* ignored by MJPEG; kept for the future H.264 path */
    uint32_t keyframe_interval; /* ignored by MJPEG: every frame is a keyframe */
    /* UTF-8 destination. The muxer streams chunks to it as they arrive and
     * patches the headers and the index when the recording finishes. */
    char output_path[260];
    /* Audio layout for the 01wb stream. When has_audio is zero the file has a
     * single video stream and the fields below are ignored. */
    uint32_t has_audio;
    uint32_t audio_sample_rate;
    uint32_t audio_channels;
    uint32_t audio_bits_per_sample;
    uint32_t audio_format_tag; /* 1 = integer PCM, 3 = IEEE float */
    /* WIC JPEG quality, 1-100. Zero means the default of 80. */
    uint32_t jpeg_quality;
    /* ABI 3: H.264 quality level, 1-100. Zero means 70. Higher keeps text
     * crisp on motion; the bitrate above stays as the ceiling. */
    uint32_t video_quality;
} RecorderEncoderConfig;

typedef struct RecorderEncoder RecorderEncoder;

/* One encoded access unit. */
typedef struct RecorderSample {
    uint32_t struct_size;
    const uint8_t* data;
    uint32_t size;
    uint32_t key_frame;
    uint64_t duration_100ns; /* presentation time, relative to the first frame */
} RecorderSample;

/* Decoder configuration record (avcC / hvcC), needed to start playback. */
typedef enum RecorderBufferKind {
    RecorderBufferSample = 0,
    RecorderBufferDecoderConfig = 1
} RecorderBufferKind;

typedef struct RecorderBuffer {
    uint32_t struct_size;
    uint32_t kind;
    const uint8_t* data;
    uint32_t size;
    uint32_t key_frame;
    uint64_t duration_100ns;
} RecorderBuffer;

/* ------------------------------------------------------------------ audio - */

typedef struct RecorderAudio RecorderAudio;

typedef struct RecorderAudioConfig {
    uint32_t struct_size;
    uint32_t reserved;
} RecorderAudioConfig;

typedef struct RecorderAudioFormat {
    uint32_t struct_size;
    uint32_t sample_rate;
    uint32_t channels;
    uint32_t bits_per_sample;
    uint32_t format_tag;   /* 1 = PCM, 0xFFFE = extensible */
    uint64_t device_id;    /* IMMDeviceEnumerator endpoint id, for diagnostics */
} RecorderAudioFormat;

/* ------------------------------------------------------------------- api -- */

/* Library version; must match RECORDER_NATIVE_ABI or the caller must refuse. */
RECORDER_API uint32_t recorder_abi_version(void);

/* Human readable name of the build, for the diagnostics report. */
RECORDER_API const char* recorder_build_info(void);

/* Fills every adapter/output the machine exposes. Returns the number written. */
RECORDER_API uint32_t recorder_enumerate_adapters(RecorderAdapterInfo* out, uint32_t capacity);

/* Fills the machine capability summary. */
RECORDER_API int32_t recorder_query_capabilities(RecorderCapabilities* out);

/* Last error of the calling thread, as UTF-8. Always NUL terminates. */
RECORDER_API uint32_t recorder_last_error(char* buffer, uint32_t capacity);

/* --- capture --- */
RECORDER_API int32_t recorder_capture_open(const RecorderCaptureConfig* config, RecorderCapture** out);
RECORDER_API int32_t recorder_capture_grab(RecorderCapture* capture, int32_t timeout_ms, RecorderFrame* frame);
RECORDER_API int32_t recorder_capture_cursor(RecorderCapture* capture, RecorderCursor* out);
/* Pixels of the frame the last grab produced, valid until the next grab. */
RECORDER_API const uint8_t* recorder_capture_pixels(const RecorderCapture* capture);
RECORDER_API uint32_t recorder_capture_backend(RecorderCapture* capture);
RECORDER_API void recorder_capture_close(RecorderCapture* capture);

/* --- encoder --- */
RECORDER_API int32_t recorder_encoder_open(const RecorderEncoderConfig* config, RecorderEncoder** out);
/* One BGRA8 frame, top-down; stride is bytes per row and may exceed width * 4.
 * The duration anchors the frame on the timeline and is currently informational:
 * one call always appends exactly one frame at the configured frame rate. */
RECORDER_API int32_t recorder_encoder_encode(RecorderEncoder* encoder,
                                const uint8_t* bgra, int32_t stride,
                                uint64_t duration_100ns);
/* One block of PCM bytes in the layout the config declared (01wb stream). */
RECORDER_API int32_t recorder_encoder_audio_write(RecorderEncoder* encoder,
                                const uint8_t* data, uint32_t size);
RECORDER_API int32_t recorder_encoder_read(RecorderEncoder* encoder, RecorderBuffer* out, uint32_t capacity);
RECORDER_API int32_t recorder_encoder_flush(RecorderEncoder* encoder);
/* Writes the MP4 or AVI container around whatever the encoder produced. */
RECORDER_API int32_t recorder_encoder_finish(RecorderEncoder* encoder, const char* path);
RECORDER_API uint32_t recorder_encoder_is_hardware(RecorderEncoder* encoder);
RECORDER_API void recorder_encoder_close(RecorderEncoder* encoder);

/* --- audio --- */
RECORDER_API int32_t recorder_audio_open(const RecorderAudioConfig* config, RecorderAudio** out);
RECORDER_API int32_t recorder_audio_read(RecorderAudio* audio, uint8_t* buffer, uint32_t capacity, int32_t* frames);
RECORDER_API int32_t recorder_audio_format(RecorderAudio* audio, RecorderAudioFormat* out);
RECORDER_API uint32_t recorder_audio_count(char** names, uint32_t capacity);
RECORDER_API void recorder_audio_close(RecorderAudio* audio);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* RECORDER_NATIVE_H */
