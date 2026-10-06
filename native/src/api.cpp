// The exported C entry points.
//
// Each one is a thin shell that converts the status, records the error and never
// lets an exception escape into Go: a throwing DLL boundary turns a recoverable
// problem into a crash. The opaque handles the header declares are cast to the
// session types here, which is the one place that knows both spellings.
#include "common.h"
#include "internal.h"

#include <new>

using recorder::AudioSession;
using recorder::CaptureSession;
using recorder::EncoderSession;

namespace {

// The C ABI and the C++ sessions describe the same object; the casts are the
// price of keeping the header free of C++ types.
RecorderCapture* toHandle(CaptureSession* session) {
    return reinterpret_cast<RecorderCapture*>(session);
}
CaptureSession* toSession(RecorderCapture* handle) {
    return reinterpret_cast<CaptureSession*>(handle);
}
RecorderEncoder* toHandle(EncoderSession* session) {
    return reinterpret_cast<RecorderEncoder*>(session);
}
EncoderSession* toSession(RecorderEncoder* handle) {
    return reinterpret_cast<EncoderSession*>(handle);
}
RecorderAudio* toHandle(AudioSession* session) {
    return reinterpret_cast<RecorderAudio*>(session);
}
AudioSession* toSession(RecorderAudio* handle) {
    return reinterpret_cast<AudioSession*>(handle);
}

}  // namespace

extern "C" {

uint32_t recorder_abi_version(void) {
    return RECORDER_NATIVE_ABI;
}

const char* recorder_build_info(void) {
    return "recorder_native 2.1 (DXGI + GDI capture, H.264 MP4 + MJPEG AVI, WASAPI loopback)";
}

uint32_t recorder_enumerate_adapters(RecorderAdapterInfo* out, uint32_t capacity) {
    try {
        return recorder::enumerateAdapters(out, capacity);
    } catch (const std::exception& e) {
        recorder::fail(RecorderStatusError, "enumerate_adapters: %s", e.what());
        return 0;
    } catch (...) {
        recorder::fail(RecorderStatusError, "enumerate_adapters: unknown failure");
        return 0;
    }
}

int32_t recorder_query_capabilities(RecorderCapabilities* out) {
    try {
        if (out == nullptr) {
            return recorder::fail(RecorderStatusError, "capabilities pointer is null");
        }
        if (out->struct_size != 0 && out->struct_size < sizeof(RecorderCapabilities)) {
            return recorder::fail(RecorderStatusError,
                                  "capabilities struct too small: %u bytes", out->struct_size);
        }
        recorder::fillCapabilities(out);
        return recorder::kOk;
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "query_capabilities: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "query_capabilities: unknown failure");
    }
}

uint32_t recorder_last_error(char* buffer, uint32_t capacity) {
    const std::string& message = recorder::lastError();
    if (buffer == nullptr || capacity == 0) {
        return static_cast<uint32_t>(message.size());
    }
    recorder::copyString(buffer, capacity, message);
    return static_cast<uint32_t>(message.size());
}

/* --- capture --- */

int32_t recorder_capture_open(const RecorderCaptureConfig* config, RecorderCapture** out) {
    try {
        CaptureSession* session = nullptr;
        const int32_t status = recorder::captureOpen(config, &session);
        *out = toHandle(session);
        return status;
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "capture_open: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "capture_open: unknown failure");
    }
}

int32_t recorder_capture_grab(RecorderCapture* capture, int32_t timeout_ms,
                              RecorderFrame* frame) {
    try {
        return recorder::captureGrab(toSession(capture), timeout_ms, frame);
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "capture_grab: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "capture_grab: unknown failure");
    }
}

int32_t recorder_capture_cursor(RecorderCapture* capture, RecorderCursor* out) {
    try {
        return recorder::captureCursor(toSession(capture), out);
    } catch (...) {
        return recorder::fail(RecorderStatusError, "capture_cursor: unknown failure");
    }
}

const uint8_t* recorder_capture_pixels(const RecorderCapture* capture) {
    return recorder::capturePixels(toSession(const_cast<RecorderCapture*>(capture)));
}

uint32_t recorder_capture_backend(RecorderCapture* capture) {
    return toSession(capture) == nullptr ? RecorderBackendNone : toSession(capture)->backend();
}

void recorder_capture_close(RecorderCapture* capture) {
    delete toSession(capture);
}

/* --- encoder --- */

int32_t recorder_encoder_open(const RecorderEncoderConfig* config, RecorderEncoder** out) {
    try {
        EncoderSession* session = nullptr;
        const int32_t status = recorder::encoderOpen(config, &session);
        *out = toHandle(session);
        return status;
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "encoder_open: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_open: unknown failure");
    }
}

int32_t recorder_encoder_encode(RecorderEncoder* encoder, const uint8_t* bgra,
                                int32_t stride, uint64_t duration_100ns) {
    try {
        return recorder::encoderEncode(toSession(encoder), bgra, stride, duration_100ns);
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "encoder_encode: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_encode: unknown failure");
    }
}

int32_t recorder_encoder_audio_write(RecorderEncoder* encoder, const uint8_t* data,
                                     uint32_t size) {
    try {
        return recorder::encoderAudioWrite(toSession(encoder), data, size);
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "encoder_audio_write: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_audio_write: unknown failure");
    }
}

int32_t recorder_encoder_read(RecorderEncoder* encoder, RecorderBuffer* out,
                              uint32_t capacity) {
    try {
        return recorder::encoderRead(toSession(encoder), out, capacity);
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_read: unknown failure");
    }
}

int32_t recorder_encoder_flush(RecorderEncoder* encoder) {
    try {
        return recorder::encoderFlush(toSession(encoder));
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_flush: unknown failure");
    }
}

int32_t recorder_encoder_finish(RecorderEncoder* encoder, const char* path) {
    try {
        return recorder::encoderFinish(toSession(encoder), path);
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "encoder_finish: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "encoder_finish: unknown failure");
    }
}

uint32_t recorder_encoder_is_hardware(RecorderEncoder* encoder) {
    return encoder != nullptr && toSession(encoder)->isHardware() ? 1u : 0u;
}

void recorder_encoder_close(RecorderEncoder* encoder) {
    delete toSession(encoder);
}

/* --- audio --- */

int32_t recorder_audio_open(const RecorderAudioConfig* config, RecorderAudio** out) {
    try {
        AudioSession* session = nullptr;
        const int32_t status = recorder::audioOpen(config, &session);
        *out = toHandle(session);
        return status;
    } catch (const std::exception& e) {
        return recorder::fail(RecorderStatusError, "audio_open: %s", e.what());
    } catch (...) {
        return recorder::fail(RecorderStatusError, "audio_open: unknown failure");
    }
}

int32_t recorder_audio_read(RecorderAudio* audio, uint8_t* buffer, uint32_t capacity,
                            int32_t* frames) {
    try {
        return recorder::audioRead(toSession(audio), buffer, capacity, frames);
    } catch (...) {
        return recorder::fail(RecorderStatusError, "audio_read: unknown failure");
    }
}

int32_t recorder_audio_format(RecorderAudio* audio, RecorderAudioFormat* out) {
    try {
        return recorder::audioFormat(toSession(audio), out);
    } catch (...) {
        return recorder::fail(RecorderStatusError, "audio_format: unknown failure");
    }
}

uint32_t recorder_audio_count(char** names, uint32_t capacity) {
    try {
        return recorder::audioCount(names, capacity);
    } catch (...) {
        return 0;
    }
}

void recorder_audio_close(RecorderAudio* audio) {
    delete toSession(audio);
}

}  // extern "C"
