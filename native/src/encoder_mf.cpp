// H.264 video (and AAC audio) into MP4 through the Media Foundation sink
// writer, with MJPEG/AVI kept as the automatic fallback.
//
// The old code drove an IMFTransform by hand: enumerate encoders, negotiate
// input types per driver, pump ProcessInput/ProcessOutput, assemble the
// decoder record. Every step was a per-machine negotiation, and a miss in any
// of them meant no file. This path does none of that: the writer is told
// "NV12 in, H.264 out" (and "PCM in, AAC out") and picks the encoder MFT
// itself — hardware when the GPU has one, the inbox software encoder
// otherwise. Capture still hands over BGRA, so this file owns the only
// conversion in the pipeline (BGRA to NV12) and the presentation timeline.
//
// Bitrate is what makes an hour fit in ~300 MB: 800 kbps of video plus
// 64 kbps of audio is ~110 KB/s, ~390 MB/h worst case, and a real desktop
// spends most of its time far below the ceiling because still frames cost
// almost nothing to a temporal codec.
#include "internal.h"

#include <codecapi.h>
#include <d3d10.h>
#include <d3d11.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfobjects.h>
#include <mfreadwrite.h>
#include <mftransform.h>
#include <wmcodecdsp.h>

#include <algorithm>
#include <cstring>
#include <mutex>
#include <string>
#include <vector>

namespace recorder {
namespace {

constexpr uint32_t kDefaultVideoBitrate = 600000;  // ~270 MB/h worst case
// The AAC encoder offers discrete bitrates starting at 96 kbps; requesting
// anything else (e.g. 64k) matches no offered output type and the writer
// refuses the stream. Probed on the encoder MFT, not guessed.
constexpr uint32_t kAudioBitrate = 96000;  // ~43 MB/h worst case

std::string hexOf(long value) {
    char buffer[16];
    snprintf(buffer, sizeof(buffer), "%08lX", static_cast<unsigned long>(value));
    return std::string(buffer);
}

// createD3DDevice builds a hardware video device for the encoder to run on.
// Only real hardware is tried: a WARP device would just launder the software
// path through D3D with nothing gained.
//
// Multithread protection is mandatory, not optional: the writer hands this
// device to driver threads while recording goroutines migrate across OS
// threads, and an unprotected immediate context corrupts the heap. That
// corruption surfaced far from its cause as a frozen window thread inside
// SetWindowTextW — the signature flaky hang this flag fixes.
bool createD3DDevice(ID3D11Device** out) {
    static const D3D_FEATURE_LEVEL levels[] = {
        D3D_FEATURE_LEVEL_12_1, D3D_FEATURE_LEVEL_12_0, D3D_FEATURE_LEVEL_11_1,
        D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1, D3D_FEATURE_LEVEL_10_0,
    };
    for (uint32_t skip = 0; skip < ARRAYSIZE(levels); ++skip) {
        ComPtr<ID3D11Device> device;
        HRESULT hr = D3D11CreateDevice(
            nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr, D3D11_CREATE_DEVICE_VIDEO_SUPPORT,
            levels + skip, ARRAYSIZE(levels) - skip, D3D11_SDK_VERSION, device.put(), nullptr,
            nullptr);
        if (FAILED(hr)) {
            if (hr != E_INVALIDARG) {
                return false;
            }
            continue;
        }
        ComPtr<ID3D10Multithread> multithreaded;
        if (queryInterface(device.get(), &multithreaded) && multithreaded) {
            multithreaded->SetMultithreadProtected(TRUE);
        }
        *out = device.detach();
        return true;
    }
    return false;
}

void ensureMediaFoundation() {
    static const bool started = [] {
        ensureCom();
        const HRESULT hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
        // E_ACCESSDENIED means a startup is already in flight elsewhere;
        // either way the platform stays up for the life of the process and
        // is never shut down, matching the ensureCom contract above.
        (void)hr;
        return true;
    }();
    (void)started;
}

inline uint8_t clampByte(int value) {
    return static_cast<uint8_t>(value < 0 ? 0 : (value > 255 ? 255 : value));
}

// bgraToNv12 repacks one top-down BGRA frame (possibly padded rows) into
// contiguous NV12. BT.601 studio swing in integer arithmetic; a 1080p frame
// costs a few milliseconds scalar, which fits comfortably in a 33 ms budget.
void bgraToNv12(const uint8_t* bgra, int32_t stride, uint32_t width, uint32_t height,
                std::vector<uint8_t>& nv12) {
    nv12.resize(static_cast<size_t>(width) * height * 3 / 2);
    uint8_t* yPlane = nv12.data();
    uint8_t* uvPlane = nv12.data() + static_cast<size_t>(width) * height;
    for (uint32_t y = 0; y < height; ++y) {
        const uint8_t* src = bgra + static_cast<size_t>(y) * stride;
        uint8_t* dst = yPlane + static_cast<size_t>(y) * width;
        for (uint32_t x = 0; x < width; ++x) {
            const int b = src[x * 4];
            const int g = src[x * 4 + 1];
            const int r = src[x * 4 + 2];
            dst[x] = clampByte(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
        }
    }
    for (uint32_t y = 0; y < height; y += 2) {
        const uint8_t* row0 = bgra + static_cast<size_t>(y) * stride;
        const uint8_t* row1 = bgra + static_cast<size_t>(y + 1) * stride;
        uint8_t* dst = uvPlane + static_cast<size_t>(y / 2) * width;
        for (uint32_t x = 0; x < width; x += 2) {
            // The chroma pair covers a 2x2 luma quad; averaging the four
            // corners is the honest downsample, not just the top-left.
            int r = 0, g = 0, b = 0;
            for (int dy = 0; dy < 2; ++dy) {
                const uint8_t* row = dy == 0 ? row0 : row1;
                for (int dx = 0; dx < 2; ++dx) {
                    const size_t o = static_cast<size_t>(x + dx) * 4;
                    b += row[o];
                    g += row[o + 1];
                    r += row[o + 2];
                }
            }
            r /= 4;
            g /= 4;
            b /= 4;
            dst[x] = clampByte(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
            dst[x + 1] = clampByte(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
        }
    }
}

// floatToS16 converts interleaved 32-bit float PCM to 16-bit PCM, which every
// AAC encoder accepts. Clipping rather than wrapping: an overdriven mix
// becomes loud, not garbage.
void floatToS16(const uint8_t* src, uint32_t frames, uint32_t channels, std::vector<uint8_t>& out) {
    out.resize(static_cast<size_t>(frames) * channels * 2);
    const auto* fs = reinterpret_cast<const float*>(src);
    auto* dst = reinterpret_cast<int16_t*>(out.data());
    for (size_t i = 0, n = static_cast<size_t>(frames) * channels; i < n; ++i) {
        float v = fs[i];
        v = v < -1.0f ? -1.0f : (v > 1.0f ? 1.0f : v);
        dst[i] = static_cast<int16_t>(v * 32767.0f);
    }
}

HRESULT setAudioInputType(IMFSinkWriter* writer, DWORD stream, uint32_t rate,
                           uint32_t channels) {
    ComPtr<IMFMediaType> input;
    HRESULT hr = MFCreateMediaType(input.put());
    if (FAILED(hr)) {
        return hr;
    }
    const uint32_t blockAlign = channels * 2;
    input->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Audio);
    input->SetGUID(MF_MT_SUBTYPE, MFAudioFormat_PCM);
    input->SetUINT32(MF_MT_AUDIO_NUM_CHANNELS, channels);
    input->SetUINT32(MF_MT_AUDIO_SAMPLES_PER_SECOND, rate);
    input->SetUINT32(MF_MT_AUDIO_AVG_BYTES_PER_SECOND, rate * blockAlign);
    input->SetUINT32(MF_MT_AUDIO_BLOCK_ALIGNMENT, blockAlign);
    input->SetUINT32(MF_MT_AUDIO_BITS_PER_SAMPLE, 16);
    return writer->SetInputMediaType(stream, input.get(), nullptr);
}

class MfMp4Session final : public EncoderSession {
  public:
    ~MfMp4Session() override = default;

    bool open(const RecorderEncoderConfig& config) {
        if (config.width == 0 || config.height == 0 || config.frames_per_second == 0) {
            mutableLastError() = "encoder_open: width, height and frame rate are required";
            return false;
        }
        if ((config.width & 1) != 0 || (config.height & 1) != 0) {
            mutableLastError() = "encoder_open: H.264 needs even dimensions";
            return false;
        }
        width_ = config.width;
        height_ = config.height;
        fps_ = config.frames_per_second;
        bitrate_ = config.bits_per_second == 0 ? kDefaultVideoBitrate : config.bits_per_second;
        quality_ = config.video_quality == 0 ? 70 : config.video_quality;
        if (quality_ > 100) {
            quality_ = 100;
        }

        if (config.has_audio != 0) {
            if (config.audio_sample_rate == 0 || config.audio_channels == 0) {
                mutableLastError() = "encoder_open: audio is enabled but its format is empty";
                return false;
            }
            const bool s16 = config.audio_format_tag == 1 && config.audio_bits_per_sample == 16;
            const bool f32 = config.audio_format_tag == 3 && config.audio_bits_per_sample == 32;
            if (!s16 && !f32) {
                mutableLastError() =
                    "encoder_open: MP4 audio wants 16-bit PCM or 32-bit float loopback";
                return false;
            }
            hasAudio_ = true;
            sampleRate_ = config.audio_sample_rate;
            channels_ = config.audio_channels;
            floatAudio_ = f32;
            blockAlignIn_ = channels_ * (f32 ? 4u : 2u);
        }

        ensureMediaFoundation();

        wchar_t path[MAX_PATH]{};
        if (MultiByteToWideChar(CP_UTF8, 0, config.output_path, -1, path, MAX_PATH) <= 0) {
            mutableLastError() = "encoder_open: the output path is empty";
            return false;
        }
        // Hardware transforms are requested explicitly and a D3D manager is
        // handed over: without it the writer settles for the software
        // encoder even when NVENC/QuickSync sit idle next to it.
        ComPtr<IMFAttributes> writerAttrs;
        UINT deviceToken = 0;
        if (SUCCEEDED(MFCreateAttributes(writerAttrs.put(), 2))) {
            writerAttrs->SetUINT32(MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS, TRUE);
            if (createD3DDevice(device_.put()) && device_) {
                if (SUCCEEDED(MFCreateDXGIDeviceManager(&deviceToken, deviceManager_.put())) &&
                    SUCCEEDED(deviceManager_->ResetDevice(device_.get(), deviceToken))) {
                    writerAttrs->SetUnknown(MF_SINK_WRITER_D3D_MANAGER, deviceManager_.get());
                }
            }
        }
        HRESULT hr =
            MFCreateSinkWriterFromURL(path, nullptr, writerAttrs.get(), writer_.put());
        if (FAILED(hr)) {
            mutableLastError() = std::string("encoder_open: the output file refused a writer (0x") +
                                 hexOf(hr) + ")";
            return false;
        }

        if (!addVideoStream()) {
            return false;
        }
        if (hasAudio_ && !addAudioStream()) {
            return false;
        }
        hr = writer_->BeginWriting();
        if (FAILED(hr)) {
            mutableLastError() = std::string("encoder_open: no H.264 encoder took the stream (0x") +
                                 hexOf(hr) + "); MJPEG remains available";
            writer_.reset();
            noEncoder_ = true;
            return false;
        }
        detectHardware();
        return true;
    }

    bool tookEncoder() const { return !noEncoder_; }

    int32_t encode(const uint8_t* bgra, int32_t stride, uint64_t /*duration100ns*/) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!writer_) {
            return fail(RecorderStatusError, "encode: the recording is closed");
        }
        if (bgra == nullptr) {
            return fail(RecorderStatusError, "encode: no pixels were supplied");
        }
        if (stride < static_cast<int32_t>(width_ * 4)) {
            return fail(RecorderStatusError, "encode: stride %d is too narrow for %u pixels",
                        stride, width_);
        }
        bgraToNv12(bgra, stride, width_, height_, nv12_);

        ComPtr<IMFMediaBuffer> buffer;
        HRESULT hr = MFCreateMemoryBuffer(static_cast<DWORD>(nv12_.size()), buffer.put());
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "encode: out of memory (0x%s)", hexOf(hr).c_str());
        }
        BYTE* dst = nullptr;
        DWORD maxLen = 0;
        if (FAILED(buffer->Lock(&dst, &maxLen, nullptr))) {
            return fail(RecorderStatusError, "encode: the frame buffer would not lock");
        }
        if (maxLen < nv12_.size()) {
            buffer->Unlock();
            return fail(RecorderStatusError, "encode: the frame buffer is too small");
        }
        memcpy(dst, nv12_.data(), nv12_.size());
        buffer->Unlock();
        buffer->SetCurrentLength(static_cast<DWORD>(nv12_.size()));

        ComPtr<IMFSample> sample;
        if (FAILED(MFCreateSample(sample.put())) || FAILED(sample->AddBuffer(buffer.get()))) {
            return fail(RecorderStatusError, "encode: the frame sample could not be built");
        }
        const LONGLONG frameDur = static_cast<LONGLONG>(10000000ull / fps_);
        sample->SetSampleTime(static_cast<LONGLONG>(videoTime_));
        sample->SetSampleDuration(frameDur);
        hr = writer_->WriteSample(videoStream_, sample.get());
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "encode: the writer refused a frame (0x%s)",
                        hexOf(hr).c_str());
        }
        videoTime_ += static_cast<uint64_t>(frameDur);
        return kOk;
    }

    int32_t audioWrite(const uint8_t* data, uint32_t size) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!writer_) {
            return fail(RecorderStatusError, "audio_write: the recording is closed");
        }
        if (!hasAudio_) {
            return fail(RecorderStatusError, "audio_write: this recording has no audio stream");
        }
        if (data == nullptr || size == 0) {
            return fail(RecorderStatusError, "audio_write: nothing was supplied");
        }
        const uint8_t* pcm = data;
        uint32_t frames = size / blockAlignIn_;
        if (frames == 0) {
            return kOk;
        }
        if (floatAudio_) {
            floatToS16(data, frames, channels_, s16_);
            pcm = s16_.data();
        }
        const uint32_t blockAlign = channels_ * 2;
        const uint32_t bytes = frames * blockAlign;

        ComPtr<IMFMediaBuffer> buffer;
        HRESULT hr = MFCreateMemoryBuffer(bytes, buffer.put());
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "audio_write: out of memory (0x%s)",
                        hexOf(hr).c_str());
        }
        BYTE* dst = nullptr;
        DWORD maxLen = 0;
        if (FAILED(buffer->Lock(&dst, &maxLen, nullptr))) {
            return fail(RecorderStatusError, "audio_write: the audio buffer would not lock");
        }
        if (maxLen < bytes) {
            buffer->Unlock();
            return fail(RecorderStatusError, "audio_write: the audio buffer is too small");
        }
        memcpy(dst, pcm, bytes);
        buffer->Unlock();
        buffer->SetCurrentLength(bytes);

        ComPtr<IMFSample> sample;
        if (FAILED(MFCreateSample(sample.put())) || FAILED(sample->AddBuffer(buffer.get()))) {
            return fail(RecorderStatusError, "audio_write: the audio sample could not be built");
        }
        const LONGLONG dur =
            static_cast<LONGLONG>(frames) * 10000000ull / sampleRate_;
        sample->SetSampleTime(static_cast<LONGLONG>(audioTime_));
        sample->SetSampleDuration(dur);
        hr = writer_->WriteSample(audioStream_, sample.get());
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "audio_write: the writer refused audio (0x%s)",
                        hexOf(hr).c_str());
        }
        audioTime_ += static_cast<uint64_t>(dur);
        return kOk;
    }

    uint32_t read(RecorderBuffer* /*out*/, uint32_t /*capacity*/) override { return 0; }

    int32_t flush() override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (writer_) {
            writer_->Flush(videoStream_);
        }
        return kOk;
    }

    int32_t finish(const char* /*path*/) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!writer_) {
            return fail(RecorderStatusError, "finish: the recording is already closed");
        }
        // Finalize writes the moov box a player needs; without it the file is
        //mdat with no index, exactly the truncation the MJPEG path avoids by
        // patching headers instead.
        const HRESULT hr = writer_->Finalize();
        writer_.reset();
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "finish: the MP4 could not be finalised (0x%s)",
                        hexOf(hr).c_str());
        }
        return kOk;
    }

    bool isHardware() const override { return hardware_; }

  private:
    bool addVideoStream() {
        ComPtr<IMFMediaType> output;
        if (FAILED(MFCreateMediaType(output.put()))) {
            mutableLastError() = "encoder_open: out of memory";
            return false;
        }
        // AddStream takes the ENCODED (target) type; the uncompressed input
        // follows through SetInputMediaType. There is no SetOutputMediaType
        // on a sink writer: the target type given here is what the writer
        // builds its internal encoder topology from.
        output->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        output->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
        output->SetUINT64(MF_MT_FRAME_SIZE,
                          (static_cast<UINT64>(width_) << 32) | height_);
        output->SetUINT64(MF_MT_FRAME_RATE,
                          (static_cast<UINT64>(fps_) << 32) | 1u);
        output->SetUINT32(MF_MT_INTERLACE_MODE, 2);  // progressive
        output->SetUINT32(MF_MT_AVG_BITRATE, bitrate_);
        HRESULT hr = writer_->AddStream(output.get(), &videoStream_);
        if (FAILED(hr)) {
            mutableLastError() = std::string("encoder_open: the container refused video (0x") +
                                 hexOf(hr) + ")";
            return false;
        }

        ComPtr<IMFMediaType> input;
        if (FAILED(MFCreateMediaType(input.put()))) {
            mutableLastError() = "encoder_open: out of memory";
            return false;
        }
        input->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        input->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
        input->SetUINT64(MF_MT_FRAME_SIZE,
                         (static_cast<UINT64>(width_) << 32) | height_);
        input->SetUINT64(MF_MT_FRAME_RATE, (static_cast<UINT64>(fps_) << 32) | 1u);
        input->SetUINT32(MF_MT_INTERLACE_MODE, 2);
        // Rate control travels as encoding parameters, not as media-type
        // attributes. Quality VBR is tried first: it keeps text crisp on
        // motion (a fixed 600k CBR turns scrolling and video into mush) while
        // spending almost nothing on the still frames a desktop mostly is.
        // AVG_BITRATE stays on the output type as the ceiling. Encoders that
        // do not know quality mode refuse the parameters, and the CBR retry
        // below keeps those machines recording instead of failing.
        ComPtr<IMFAttributes> encoding;
        IMFAttributes* params = nullptr;
        if (SUCCEEDED(MFCreateAttributes(encoding.put(), 2))) {
            encoding->SetUINT32(CODECAPI_AVEncCommonRateControlMode,
                                static_cast<UINT32>(eAVEncCommonRateControlMode_Quality));
            encoding->SetUINT32(CODECAPI_AVEncCommonQuality, quality_);
            params = encoding.get();
        }
        hr = writer_->SetInputMediaType(videoStream_, input.get(), params);
        if (FAILED(hr) && params != nullptr) {
            encoding->SetUINT32(CODECAPI_AVEncCommonRateControlMode,
                                static_cast<UINT32>(eAVEncCommonRateControlMode_CBR));
            hr = writer_->SetInputMediaType(videoStream_, input.get(), params);
        }
        if (FAILED(hr)) {
            mutableLastError() = std::string("encoder_open: NV12 was refused (0x") + hexOf(hr) +
                                 ")";
            return false;
        }
        return true;
    }

    bool addAudioStream() {
        ComPtr<IMFMediaType> output;
        if (FAILED(MFCreateMediaType(output.put()))) {
            mutableLastError() = "encoder_open: out of memory";
            return false;
        }
        output->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Audio);
        output->SetGUID(MF_MT_SUBTYPE, MFAudioFormat_AAC);
        output->SetUINT32(MF_MT_AUDIO_NUM_CHANNELS, channels_);
        output->SetUINT32(MF_MT_AUDIO_SAMPLES_PER_SECOND, sampleRate_);
        output->SetUINT32(MF_MT_AVG_BITRATE, kAudioBitrate);
        // Raw AAC frames in MP4, not ADTS: the container carries the lengths.
        output->SetUINT32(MF_MT_AAC_PAYLOAD_TYPE, 0);
        // A compressed stream has no block structure; stating 1 keeps strict
        // type matchers from rejecting the description as incomplete.
        output->SetUINT32(MF_MT_AUDIO_BLOCK_ALIGNMENT, 1);
        HRESULT hr = writer_->AddStream(output.get(), &audioStream_);
        if (FAILED(hr)) {
            mutableLastError() = std::string("encoder_open: the container refused audio (0x") +
                                 hexOf(hr) + ")";
            return false;
        }
        const HRESULT inHr =
            setAudioInputType(writer_.get(), audioStream_, sampleRate_, channels_);
        if (FAILED(inHr)) {
            mutableLastError() = std::string("encoder_open: the AAC input was refused (0x") +
                                 hexOf(inHr) + "); no AAC encoder took the audio stream";
            return false;
        }
        return true;
    }

    // detectHardware asks whether a hardware H.264 MFT exists, for reporting
    // only: the writer already chose its encoder, and this answer never gates
    // the recording.
    void detectHardware() {
        const MFT_REGISTER_TYPE_INFO wantH264 = {MFMediaType_Video, MFVideoFormat_H264};
        IMFActivate** found = nullptr;
        UINT32 count = 0;
        const HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                                     MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                                     nullptr, &wantH264, &found, &count);
        hardware_ = SUCCEEDED(hr) && count > 0;
        if (found != nullptr) {
            CoTaskMemFree(found);
        }
    }

    ComPtr<IMFSinkWriter> writer_;
    // The D3D device and its manager are held for the session: the writer
    // only borrows them, and a borrowed D3D device has no business
    // outliving its lender.
    ComPtr<IMFDXGIDeviceManager> deviceManager_;
    ComPtr<ID3D11Device> device_;
    DWORD videoStream_ = 0;
    DWORD audioStream_ = 0;
    std::vector<uint8_t> nv12_;
    std::vector<uint8_t> s16_;
    std::mutex mutex_;
    uint32_t width_ = 0;
    uint32_t height_ = 0;
    uint32_t fps_ = 0;
    uint32_t bitrate_ = 0;
    uint32_t quality_ = 70;
    uint64_t videoTime_ = 0;
    uint64_t audioTime_ = 0;
    bool hasAudio_ = false;
    bool floatAudio_ = false;
    uint32_t sampleRate_ = 0;
    uint32_t channels_ = 0;
    uint32_t blockAlignIn_ = 0;
    bool hardware_ = false;
    bool noEncoder_ = false;
};

}  // namespace

int32_t mfEncoderOpen(const RecorderEncoderConfig* config, EncoderSession** out) {
    if (config == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "encoder_open: null argument");
    }
    *out = nullptr;
    auto* session = new (std::nothrow) MfMp4Session();
    if (session == nullptr) {
        return fail(RecorderStatusError, "encoder_open: out of memory");
    }
    if (!session->open(*config)) {
        // noEncoder_ is set only where BeginWriting reports that no encoder
        // took the stream, so that one failure degrades to the MJPEG fallback
        // while every other failure (bad parameters, no disk) stays an error.
        const bool missing = !session->tookEncoder();
        const std::string why = lastError();
        delete session;
        if (missing && !why.empty()) {
            return fail(RecorderStatusUnsupported, "%s", why.c_str());
        }
        if (why.empty()) {
            return fail(RecorderStatusError, "the H.264 recording could not be started");
        }
        return fail(RecorderStatusError, "%s", why.c_str());
    }
    *out = session;
    return kOk;
}

}  // namespace recorder
