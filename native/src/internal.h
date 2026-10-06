// Declarations shared between the native translation units. The C ABI is in
// recorder_native.h; these are the C++ shapes behind it.
#pragma once

#include <d3d11.h>
#include <dxgi1_2.h>

#include <memory>
#include <string>
#include <vector>

#include "common.h"

namespace recorder {

// --- devices (devices.cpp) ---
std::vector<RecorderAdapterInfo> enumerateAdapters();
uint32_t enumerateAdapters(RecorderAdapterInfo* out, uint32_t capacity);
void fillCapabilities(RecorderCapabilities* out);
// Shared with the capture backends: duplication needs exactly the device logic
// that discovery used to decide whether duplication is available at all, so it
// is one implementation rather than two that can disagree.
ComPtr<ID3D11Device> deviceOnAdapter(IDXGIAdapter1* adapter);
bool canDuplicate(IDXGIAdapter1* adapter, uint32_t outputIndex);

// --- capture (capture.cpp) ---
struct CaptureSession {
    virtual ~CaptureSession() = default;
    virtual int32_t grab(int32_t timeoutMs) = 0;
    virtual void frame(RecorderFrame* out) const = 0;
    virtual void cursor(RecorderCursor* out) const = 0;
    virtual uint32_t backend() const = 0;
    // BGRA8 pixels, valid until the next grab. Go reads them in place rather
    // than copying every frame across the ABI.
    virtual const uint8_t* pixels() const = 0;
};

int32_t captureOpen(const RecorderCaptureConfig* config, CaptureSession** out);
int32_t captureGrab(CaptureSession* capture, int32_t timeoutMs, RecorderFrame* out);
int32_t captureCursor(CaptureSession* capture, RecorderCursor* out);
const uint8_t* capturePixels(const CaptureSession* capture);

// --- encoder (encoder.cpp) ---
struct EncoderSession {
    virtual ~EncoderSession() = default;
    virtual int32_t encode(const uint8_t* bgra, int32_t stride, uint64_t duration100ns) = 0;
    virtual int32_t audioWrite(const uint8_t* data, uint32_t size) = 0;
    virtual uint32_t read(RecorderBuffer* out, uint32_t capacity) = 0;
    virtual int32_t flush() = 0;
    virtual int32_t finish(const char* path) = 0;
    virtual bool isHardware() const = 0;
};

int32_t encoderOpen(const RecorderEncoderConfig* config, EncoderSession** out);
// H.264 video (plus AAC audio) into MP4, implemented in encoder_mf.cpp. The
// dispatcher above routes H.264 there and MJPEG at the same call below.
int32_t mfEncoderOpen(const RecorderEncoderConfig* config, EncoderSession** out);
int32_t encoderEncode(EncoderSession* encoder, const uint8_t* bgra, int32_t stride,
                      uint64_t duration100ns);
int32_t encoderAudioWrite(EncoderSession* encoder, const uint8_t* data, uint32_t size);
int32_t encoderRead(EncoderSession* encoder, RecorderBuffer* out, uint32_t capacity);
int32_t encoderFlush(EncoderSession* encoder);
int32_t encoderFinish(EncoderSession* encoder, const char* path);

// --- audio (audio.cpp) ---
struct AudioSession {
    virtual ~AudioSession() = default;
    virtual int32_t read(uint8_t* buffer, uint32_t capacity, int32_t* frames) = 0;
    virtual void format(RecorderAudioFormat* out) const = 0;
};

int32_t audioOpen(const RecorderAudioConfig* config, AudioSession** out);
int32_t audioRead(AudioSession* audio, uint8_t* buffer, uint32_t capacity, int32_t* frames);
int32_t audioFormat(AudioSession* audio, RecorderAudioFormat* out);
uint32_t audioCount(char** names, uint32_t capacity);

}  // namespace recorder
