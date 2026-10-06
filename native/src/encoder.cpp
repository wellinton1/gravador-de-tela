// Motion JPEG into AVI: one WIC JPEG per frame, muxed by a hand-rolled writer.
//
// Nothing here passes through Media Foundation. IMFTransform is the fragile
// link it replaces: opaque input negotiation, per-driver output types and a
// decoder configuration record that never arrives are what made H.264
// recordings fail on machines that should have worked. WIC JPEG encoding and
// the AVI container need no negotiation at all, so this path works everywhere
// WIC exists, which is every Windows since Vista.
//
// Every JPEG frame is a keyframe, so seeking is trivial and any player that
// opens AVI/MJPEG — Windows Media Player, VLC, ffplay — plays the file.
//
// Layout (plain AVI 1.0, no OpenDML extensions; files are limited to 4 GB):
//   RIFF 'AVI '
//     LIST 'hdrl'   avih + one strl per stream (video 'MJPG', audio PCM)
//     LIST 'movi'   '00dc' video chunks + '01wb' audio chunks, arrival order
//     'idx1'        one entry per chunk; offsets point four bytes before the
//                   chunk data, i.e. at the chunk's size field. That is the
//                   convention ffmpeg's muxer writes and Media Foundation's
//                   splitter reads: offsets pointing at the data itself make
//                   MF enumerate zero samples while every scanner-based player
//                   still plays the file, which is a nasty split to debug.
//
// Chunks stream to disk as they arrive and the headers carry counts that are
// patched when the recording finishes, so the media data is on disk even if
// the process dies before finish() runs.
#include "internal.h"

#include <objbase.h>
#include <ocidl.h>
#include <ole2.h>
#include <wincodec.h>

#include <cstring>
#include <mutex>
#include <string>
#include <vector>

namespace recorder {
namespace {

// fourcc builds a chunk identifier from four characters. AVI integers are
// little-endian, so writing the bytes in this order is the value.
constexpr uint32_t fourcc(char a, char b, char c, char d) {
    return static_cast<uint32_t>(static_cast<uint8_t>(a)) |
           (static_cast<uint32_t>(static_cast<uint8_t>(b)) << 8) |
           (static_cast<uint32_t>(static_cast<uint8_t>(c)) << 16) |
           (static_cast<uint32_t>(static_cast<uint8_t>(d)) << 24);
}

constexpr uint32_t kFourccMJPG = fourcc('M', 'J', 'P', 'G');
// HASINDEX: an idx1 is written at finish. ISINTERLEAVED: chunks of all
// streams share one movi list in arrival order. TRUSTCKTYPE: chunk tags are
// written by this muxer and mean what they say, so no content sniffing.
constexpr uint32_t kAvihFlags = 0x00000010 | 0x00000100 | 0x00000800;
// A strh chunk carries 56 bytes: the stream type and handler, then flags,
// priority and language (a DWORD followed by two WORDs), eight more DWORDs
// through dwSampleSize, and the RECT truncated to left and top. The trap is
// wPriority and wLanguage: writing them as DWORDs shifts every field after
// them by four bytes, so the reader sees dwScale = 0 and rejects the file.
// The RECT's right and bottom are dropped the way ffmpeg writes them;
// players take dimensions from strf.
constexpr uint32_t kStrhSize = 56;

std::string hexOf(long value) {
    char buffer[16];
    snprintf(buffer, sizeof(buffer), "%08lX", static_cast<unsigned long>(value));
    return std::string(buffer);
}

// One idx1 row: which chunk, how it may be used, where it is, how long it is.
struct IndexEntry {
    char tag[4];
    uint32_t flags;
    uint32_t offset;  // movi-relative, pointing at the chunk's size field
    uint32_t size;
};

class MjpegAviSession final : public EncoderSession {
  public:
    ~MjpegAviSession() override { closeFile(); }

    bool open(const RecorderEncoderConfig& config) {
        if (config.width == 0 || config.height == 0 || config.frames_per_second == 0) {
            mutableLastError() = "encoder_open: width, height and frame rate are required";
            return false;
        }
        width_ = config.width;
        height_ = config.height;
        fps_ = config.frames_per_second;
        quality_ = config.jpeg_quality == 0 ? 80 : config.jpeg_quality;
        if (quality_ > 100) {
            quality_ = 100;
        }

        if (config.has_audio != 0) {
            if (config.audio_sample_rate == 0 || config.audio_channels == 0 ||
                config.audio_bits_per_sample == 0) {
                mutableLastError() = "encoder_open: audio is enabled but its format is empty";
                return false;
            }
            if (config.audio_format_tag != 1 && config.audio_format_tag != 3) {
                mutableLastError() = "encoder_open: audio must be integer PCM (1) or IEEE float (3)";
                return false;
            }
            if (config.audio_format_tag == 3 && config.audio_bits_per_sample != 32) {
                mutableLastError() = "encoder_open: float audio must be 32-bit";
                return false;
            }
            hasAudio_ = true;
            sampleRate_ = config.audio_sample_rate;
            channels_ = config.audio_channels;
            bits_ = config.audio_bits_per_sample;
            formatTag_ = config.audio_format_tag;
            blockAlign_ = channels_ * bits_ / 8;
            avgBytes_ = sampleRate_ * blockAlign_;
        }

        wchar_t path[MAX_PATH]{};
        if (MultiByteToWideChar(CP_UTF8, 0, config.output_path, -1, path, MAX_PATH) <= 0) {
            mutableLastError() = "encoder_open: the output path is empty";
            return false;
        }
        file_ = CreateFileW(path, GENERIC_READ | GENERIC_WRITE, 0, nullptr, CREATE_ALWAYS,
                             FILE_ATTRIBUTE_NORMAL, nullptr);
        if (file_ == INVALID_HANDLE_VALUE) {
            mutableLastError() =
                std::string("encoder_open: could not create the output file (error ") +
                std::to_string(GetLastError()) + ")";
            return false;
        }

        {
            // The factory outlives this call, so COM must stay up: see ensureCom.
            ensureCom();
            ComPtr<IWICImagingFactory> factory;
            if (FAILED(CoCreateInstance(CLSID_WICImagingFactory, nullptr, CLSCTX_INPROC_SERVER,
                                        __uuidof(IWICImagingFactory),
                                        reinterpret_cast<void**>(factory.put())))) {
                mutableLastError() = "encoder_open: the WIC imaging factory is unavailable";
                closeFile();
                return false;
            }
            factory_.attach(factory.detach());
        }

        if (!writeHeaders()) {
            closeFile();
            return false;
        }
        return true;
    }

    int32_t encode(const uint8_t* bgra, int32_t stride, uint64_t /*duration100ns*/) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (file_ == INVALID_HANDLE_VALUE) {
            return fail(RecorderStatusError, "encode: the recording is closed");
        }
        if (bgra == nullptr) {
            return fail(RecorderStatusError, "encode: no pixels were supplied");
        }
        if (stride < static_cast<int32_t>(width_ * 4)) {
            return fail(RecorderStatusError, "encode: stride %d is too narrow for %u pixels",
                        stride, width_);
        }

        std::vector<uint8_t> jpeg;
        {
            // COM belongs to the calling thread, and the recorder's goroutine is
            // free to migrate between calls, so every entry point initialises it.
            ensureCom();
            if (!compressFrame(bgra, stride, &jpeg)) {
                return RecorderStatusError;  // compressFrame already recorded why
            }
        }
        if (jpeg.size() > maxJpeg_) {
            maxJpeg_ = static_cast<uint32_t>(jpeg.size());
        }
        uint32_t offset = 0;
        if (!writeChunk("00dc", jpeg.data(), static_cast<uint32_t>(jpeg.size()), &offset)) {
            return RecorderStatusError;
        }
        IndexEntry entry{};
        memcpy(entry.tag, "00dc", 4);
        entry.flags = 0x00000010;  // every JPEG frame stands on its own
        entry.offset = offset;
        entry.size = static_cast<uint32_t>(jpeg.size());
        index_.push_back(entry);
        videoFrames_++;
        return kOk;
    }

    int32_t audioWrite(const uint8_t* data, uint32_t size) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (file_ == INVALID_HANDLE_VALUE) {
            return fail(RecorderStatusError, "audio_write: the recording is closed");
        }
        if (!hasAudio_) {
            return fail(RecorderStatusError, "audio_write: this recording has no audio stream");
        }
        if (data == nullptr || size == 0) {
            return fail(RecorderStatusError, "audio_write: nothing was supplied");
        }
        uint32_t offset = 0;
        if (!writeChunk("01wb", data, size, &offset)) {
            return RecorderStatusError;
        }
        IndexEntry entry{};
        memcpy(entry.tag, "01wb", 4);
        entry.flags = 0;
        entry.offset = offset;
        entry.size = size;
        index_.push_back(entry);
        audioBytes_ += size;
        return kOk;
    }

    uint32_t read(RecorderBuffer* /*out*/, uint32_t /*capacity*/) override {
        // Streaming design: chunks go straight to disk, so there is never
        // anything queued to hand back. The method stays because the C ABI has
        // it; it simply reports an empty queue.
        return 0;
    }

    int32_t flush() override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (file_ != INVALID_HANDLE_VALUE) {
            FlushFileBuffers(file_);
        }
        return kOk;
    }

    int32_t finish(const char* /*path*/) override {
        std::lock_guard<std::mutex> lock(mutex_);
        if (file_ == INVALID_HANDLE_VALUE) {
            return fail(RecorderStatusError, "finish: the recording is already closed");
        }
        // An empty recording still gets valid headers and an empty index, so
        // the file opens as a zero-length clip instead of a truncated one.
        const uint64_t endPos = filePos_;
        const uint32_t moviSize = static_cast<uint32_t>(endPos - (moviSizePos_ + 4));
        if (!patchU32(moviSizePos_, moviSize)) {
            return RecorderStatusError;
        }

        if (!writeFourcc("idx1") ||
            !writeU32(static_cast<uint32_t>(index_.size() * sizeof(IndexEntry)))) {
            return RecorderStatusError;
        }
        for (const IndexEntry& entry : index_) {
            if (!writeRaw(entry.tag, 4) || !writeU32(entry.flags) || !writeU32(entry.offset) ||
                !writeU32(entry.size)) {
                return RecorderStatusError;
            }
        }

        const uint32_t riffSize = static_cast<uint32_t>(filePos_ - 8);
        const uint32_t moviBytes = static_cast<uint32_t>(endPos - moviDataStart_);
        const uint32_t maxBytesPerSec =
            videoFrames_ == 0 ? 0 : moviBytes / videoFrames_ * fps_;
        if (!patchU32(riffSizePos_, riffSize) || !patchU32(avihTotalPos_, videoFrames_) ||
            !patchU32(avihMaxBytesPos_, maxBytesPerSec) ||
            !patchU32(strhVideoLengthPos_, videoFrames_) ||
            !patchU32(strhVideoSuggestedPos_, maxJpeg_ == 0 ? width_ * height_ * 3 : maxJpeg_) ||
            !patchU32(strfVideoImagePos_, maxJpeg_)) {
            return RecorderStatusError;
        }
        if (hasAudio_) {
            const uint32_t audioFrames =
                blockAlign_ == 0 ? 0 : static_cast<uint32_t>(audioBytes_ / blockAlign_);
            if (!patchU32(strhAudioLengthPos_, audioFrames) ||
                !patchU32(strhAudioSuggestedPos_, avgBytes_)) {
                return RecorderStatusError;
            }
        }

        FlushFileBuffers(file_);
        closeFile();
        return kOk;
    }

    bool isHardware() const override { return false; }

  private:
    // compressFrame turns one top-down BGRA frame into a JPEG image in memory.
    // The pixels go through an explicit conversion to 24-bit BGR first, because
    // that is the input the JPEG encoder accepts without argument on every
    // Windows release; asking it to take BGRA works on some and fails on others.
    bool compressFrame(const uint8_t* bgra, int32_t stride, std::vector<uint8_t>* jpeg) {
        ComPtr<IStream> stream;
        if (FAILED(CreateStreamOnHGlobal(nullptr, TRUE, stream.put()))) {
            mutableLastError() = "encode: out of memory for the JPEG stream";
            return false;
        }
        ComPtr<IWICBitmapEncoder> encoder;
        if (FAILED(factory_->CreateEncoder(GUID_ContainerFormatJpeg, nullptr,
                                           encoder.put()))) {
            mutableLastError() = "encode: the WIC JPEG encoder is unavailable";
            return false;
        }
        if (FAILED(encoder->Initialize(stream.get(), WICBitmapEncoderNoCache))) {
            mutableLastError() = "encode: the JPEG encoder would not initialise";
            return false;
        }
        ComPtr<IWICBitmapFrameEncode> frame;
        ComPtr<IPropertyBag2> bag;
        if (FAILED(encoder->CreateNewFrame(frame.put(), bag.put()))) {
            mutableLastError() = "encode: the JPEG encoder produced no frame";
            return false;
        }
        // ImageQuality is 0..1; the bag is optional, so a failure to set it
        // only loses control over the size, never the frame.
        PROPBAG2 option{};
        option.pstrName = const_cast<LPOLESTR>(L"ImageQuality");
        VARIANT value{};
        value.vt = VT_R4;
        value.fltVal = static_cast<float>(quality_) / 100.0f;
        if (bag) {
            bag->Write(1, &option, &value);
        }
        if (FAILED(frame->Initialize(bag.get()))) {
            mutableLastError() = "encode: the JPEG frame would not initialise";
            return false;
        }
        if (FAILED(frame->SetSize(width_, height_))) {
            mutableLastError() = "encode: the JPEG frame refused its size";
            return false;
        }
        GUID wanted = GUID_WICPixelFormat24bppBGR;
        if (FAILED(frame->SetPixelFormat(&wanted)) ||
            !(wanted == GUID_WICPixelFormat24bppBGR)) {
            mutableLastError() = "encode: the JPEG encoder refused 24-bit BGR";
            return false;
        }

        ComPtr<IWICBitmap> bitmap;
        const UINT bufSize = static_cast<UINT>(stride) * height_;
        if (FAILED(factory_->CreateBitmapFromMemory(width_, height_,
                                                    GUID_WICPixelFormat32bppBGRA,
                                                    static_cast<UINT>(stride), bufSize,
                                                    const_cast<BYTE*>(bgra), bitmap.put()))) {
            mutableLastError() = "encode: the frame could not be wrapped for WIC";
            return false;
        }
        ComPtr<IWICFormatConverter> converter;
        if (FAILED(factory_->CreateFormatConverter(converter.put())) ||
            FAILED(converter->Initialize(bitmap.get(), GUID_WICPixelFormat24bppBGR,
                                         WICBitmapDitherTypeNone, nullptr, 0.0,
                                         WICBitmapPaletteTypeCustom))) {
            mutableLastError() = "encode: BGRA could not be converted for JPEG";
            return false;
        }
        if (FAILED(frame->WriteSource(converter.get(), nullptr)) ||
            FAILED(frame->Commit()) || FAILED(encoder->Commit())) {
            mutableLastError() = "encode: the JPEG frame could not be written";
            return false;
        }

        HGLOBAL locked = nullptr;
        if (FAILED(GetHGlobalFromStream(stream.get(), &locked)) || locked == nullptr) {
            mutableLastError() = "encode: the JPEG image could not be read back";
            return false;
        }
        const SIZE_T size = GlobalSize(locked);
        const void* data = GlobalLock(locked);
        if (data == nullptr || size == 0 || size > 64 * 1024 * 1024) {
            if (data != nullptr) {
                GlobalUnlock(locked);
            }
            mutableLastError() = "encode: the JPEG image came back empty";
            return false;
        }
        jpeg->assign(static_cast<const uint8_t*>(data),
                     static_cast<const uint8_t*>(data) + size);
        GlobalUnlock(locked);
        return true;
    }

    bool writeHeaders() {
        // RIFF 'AVI ' then the header list; both sizes are patched at finish().
        if (!writeFourcc("RIFF")) {
            return false;
        }
        riffSizePos_ = filePos_;
        if (!writeU32(0) || !writeFourcc("AVI ")) {
            return false;
        }

        if (!writeFourcc("LIST")) {
            return false;
        }
        const uint64_t hdrlSizePos = filePos_;
        if (!writeU32(0) || !writeFourcc("hdrl")) {
            return false;
        }

        // avih: the global header. Counts are patched at finish().
        const uint64_t avihPos = filePos_;
        const uint32_t microSecPerFrame = 1000000 / (fps_ == 0 ? 30 : fps_);
        if (!writeFourcc("avih") || !writeU32(56) || !writeU32(microSecPerFrame)) {
            return false;
        }
        avihMaxBytesPos_ = filePos_;
        if (!writeU32(0) ||                    // dwMaxBytesPerSec, patched
            !writeU32(0) ||                    // dwPaddingGranularity
            !writeU32(kAvihFlags) ||           // dwFlags
            false) {
            return false;
        }
        avihTotalPos_ = filePos_;
        const uint32_t streams = hasAudio_ ? 2u : 1u;
        if (!writeU32(0) ||                    // dwTotalFrames, patched
            !writeU32(0) ||                    // dwInitialFrames
            !writeU32(streams) ||              // dwStreams
            !writeU32(width_ * height_ * 3) ||  // dwSuggestedBufferSize
            !writeU32(width_) || !writeU32(height_) || !writeU32(0) || !writeU32(0) ||
            !writeU32(0) || !writeU32(0)) {    // dwReserved[4]
            return false;
        }
        (void)avihPos;

        // Video stream list: MJPG over vids.
        if (!writeFourcc("LIST") || !writeU32(4 + 8 + kStrhSize + 8 + 40) ||
            !writeFourcc("strl")) {
            return false;
        }
        const uint64_t strhVideoPos = filePos_;
        if (!writeFourcc("strh") || !writeU32(kStrhSize) || !writeFourcc("vids") ||
            !writeU32(kFourccMJPG) || !writeU32(0) ||  // dwFlags
            !writeU16(0) || !writeU16(0) ||            // wPriority, wLanguage
            !writeU32(0) ||                            // dwInitialFrames
            !writeU32(1) || !writeU32(fps_) || !writeU32(0)) {  // scale, rate, start
            return false;
        }
        strhVideoLengthPos_ = filePos_;
        if (!writeU32(0) ||  // dwLength in frames, patched
            false) {
            return false;
        }
        strhVideoSuggestedPos_ = filePos_;
        if (!writeU32(width_ * height_ * 3) ||  // dwSuggestedBufferSize, patched
            !writeU32(0xFFFFFFFF) ||           // dwQuality: default
            !writeU32(0) ||                    // dwSampleSize: frames vary
            !writeU32(0) || !writeU32(0)) {    // rcFrame left and top
            return false;
        }
        (void)strhVideoPos;

        const uint64_t strfVideoPos = filePos_;
        // A DWORD holds planes in its low word and the bit count in its high
        // word; swapping them describes a 24-plane 1-bit format no splitter
        // accepts, so the order here matters more than it looks.
        if (!writeFourcc("strf") || !writeU32(40) || !writeU32(40) || !writeU32(width_) ||
            !writeU32(height_) || !writeU32((24u << 16) | 1u)) {
            return false;
        }
        if (!writeU32(kFourccMJPG)) {
            return false;
        }
        strfVideoImagePos_ = filePos_;
        if (!writeU32(width_ * height_ * 3) ||  // biSizeImage, patched
            !writeU32(0) || !writeU32(0) || !writeU32(0) || !writeU32(0)) {
            return false;
        }
        (void)strfVideoPos;

        if (hasAudio_) {
            // Audio stream list: raw PCM over auds. dwScale/dwRate carry the
            // byte rate in units of one block, so dwLength counts blocks.
            if (!writeFourcc("LIST") || !writeU32(4 + 8 + kStrhSize + 8 + 18) ||
                !writeFourcc("strl")) {
                return false;
            }
            // An audio stream handler is zero; the flags word after it is a
            // separate zero. Writing only one (as this code once did) drops a
            // DWORD and shifts every field that follows, which desynchronises
            // the hdrl exactly the way a wrong strh size does.
            if (!writeFourcc("strh") || !writeU32(kStrhSize) || !writeFourcc("auds") ||
                !writeU32(0) ||                    // fccHandler: none for PCM
                !writeU32(0) ||                    // dwFlags
                !writeU16(0) || !writeU16(0) ||    // wPriority, wLanguage
                !writeU32(0) ||                    // dwInitialFrames
                !writeU32(blockAlign_) || !writeU32(avgBytes_) || !writeU32(0)) {
                return false;
            }
            strhAudioLengthPos_ = filePos_;
            if (!writeU32(0)) {  // dwLength in blocks, patched
                return false;
            }
            strhAudioSuggestedPos_ = filePos_;
            if (!writeU32(avgBytes_) || !writeU32(0xFFFFFFFF) || !writeU32(blockAlign_) ||
                !writeU32(0) || !writeU32(0)) {  // rcFrame left and top, unused
                return false;
            }
            // WAVEFORMATEX with cbSize = 0: 18 bytes.
            if (!writeFourcc("strf") || !writeU32(18) ||
                !writeU16(static_cast<uint16_t>(formatTag_)) ||
                !writeU16(static_cast<uint16_t>(channels_)) || !writeU32(sampleRate_) ||
                !writeU32(avgBytes_) || !writeU16(static_cast<uint16_t>(blockAlign_)) ||
                !writeU16(static_cast<uint16_t>(bits_)) || !writeU16(0)) {
                return false;
            }
        }

        if (!patchU32(hdrlSizePos, static_cast<uint32_t>(filePos_ - (hdrlSizePos + 4)))) {
            return false;
        }

        if (!writeFourcc("LIST")) {
            return false;
        }
        moviSizePos_ = filePos_;
        if (!writeU32(0) || !writeFourcc("movi")) {
            return false;
        }
        moviDataStart_ = filePos_;
        return true;
    }

    bool writeRaw(const void* data, uint32_t size) {
        if (size == 0) {
            return true;
        }
        DWORD done = 0;
        if (!WriteFile(file_, data, size, &done, nullptr) || done != size) {
            mutableLastError() = std::string("the recording could not be written (error ") +
                                 std::to_string(GetLastError()) + ")";
            return false;
        }
        filePos_ += size;
        return true;
    }

    bool writeU16(uint16_t value) { return writeRaw(&value, sizeof(value)); }
    bool writeU32(uint32_t value) { return writeRaw(&value, sizeof(value)); }
    bool writeFourcc(const char tag[4]) { return writeRaw(tag, 4); }

    // writeChunk appends a movi chunk and reports its idx1 offset, which
    // points at the chunk's size field: four bytes before the data. See the
    // layout note at the top of this file for why the obvious data-relative
    // offset makes Media Foundation find nothing.
    bool writeChunk(const char tag[4], const void* data, uint32_t size, uint32_t* offsetOut) {
        if (!writeFourcc(tag) || !writeU32(size)) {
            return false;
        }
        // Eight header bytes were just written, so filePos_ is at the data;
        // backing off four lands on the size field. Underflow is impossible
        // here because a chunk always follows the movi header.
        *offsetOut = static_cast<uint32_t>(filePos_ - moviDataStart_) - 4;
        if (!writeRaw(data, size)) {
            return false;
        }
        // Chunks are WORD aligned; an odd payload costs one pad byte that the
        // size field does not count.
        if ((size & 1) != 0) {
            const uint8_t zero = 0;
            if (!writeRaw(&zero, 1)) {
                return false;
            }
        }
        return true;
    }

    bool patchU32(uint64_t pos, uint32_t value) {
        LARGE_INTEGER target{};
        target.QuadPart = static_cast<LONGLONG>(pos);
        if (!SetFilePointerEx(file_, target, nullptr, FILE_BEGIN)) {
            mutableLastError() = "finish: the file headers could not be patched";
            return false;
        }
        DWORD done = 0;
        if (!WriteFile(file_, &value, sizeof(value), &done, nullptr) ||
            done != sizeof(value)) {
            mutableLastError() = "finish: the file headers could not be patched";
            return false;
        }
        LARGE_INTEGER end{};
        end.QuadPart = static_cast<LONGLONG>(filePos_);
        SetFilePointerEx(file_, end, nullptr, FILE_BEGIN);
        return true;
    }

    void closeFile() {
        if (file_ != INVALID_HANDLE_VALUE) {
            CloseHandle(file_);
            file_ = INVALID_HANDLE_VALUE;
        }
        factory_.reset();
    }

    ComPtr<IWICImagingFactory> factory_;
    HANDLE file_ = INVALID_HANDLE_VALUE;
    uint64_t filePos_ = 0;
    uint64_t moviDataStart_ = 0;
    uint64_t moviSizePos_ = 0;
    uint64_t riffSizePos_ = 0;
    uint64_t avihTotalPos_ = 0;
    uint64_t avihMaxBytesPos_ = 0;
    uint64_t strhVideoLengthPos_ = 0;
    uint64_t strhVideoSuggestedPos_ = 0;
    uint64_t strfVideoImagePos_ = 0;
    uint64_t strhAudioLengthPos_ = 0;
    uint64_t strhAudioSuggestedPos_ = 0;
    std::vector<IndexEntry> index_;
    std::mutex mutex_;
    uint32_t width_ = 0;
    uint32_t height_ = 0;
    uint32_t fps_ = 0;
    uint32_t quality_ = 80;
    bool hasAudio_ = false;
    uint32_t sampleRate_ = 0;
    uint32_t channels_ = 0;
    uint32_t bits_ = 0;
    uint32_t formatTag_ = 1;
    uint32_t blockAlign_ = 0;
    uint32_t avgBytes_ = 0;
    uint32_t videoFrames_ = 0;
    uint32_t maxJpeg_ = 0;
    uint64_t audioBytes_ = 0;
};

}  // namespace

int32_t encoderOpen(const RecorderEncoderConfig* config, EncoderSession** out) {
    if (config == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "encoder_open: null argument");
    }
    if (config->struct_size != 0 && config->struct_size < sizeof(RecorderEncoderConfig)) {
        return fail(RecorderStatusError, "encoder_open: config struct too small (%u bytes)",
                    config->struct_size);
    }
    // H.264 in MP4 is the preferred path and MJPEG in AVI the fallback; every
    // other combination is refused so the caller picks a lane that exists.
    if (config->codec == RecorderCodecH264 && config->container == RecorderContainerMp4) {
        return mfEncoderOpen(config, out);
    }
    if (config->codec == RecorderCodecH265) {
        return fail(RecorderStatusUnsupported, "H.265 has no encoder in this build");
    }
    if (config->codec != RecorderCodecMjpeg || config->container != RecorderContainerAvi) {
        return fail(RecorderStatusUnsupported,
                    "this build encodes H.264 into MP4 or Motion JPEG into AVI");
    }
    *out = nullptr;

    auto* session = new (std::nothrow) MjpegAviSession();
    if (session == nullptr) {
        return fail(RecorderStatusError, "encoder_open: out of memory");
    }
    if (!session->open(*config)) {
        const std::string why = lastError();
        delete session;
        if (why.empty()) {
            return fail(RecorderStatusError, "the recording could not be started");
        }
        // open() already recorded the reason; fail() with Error would overwrite
        // it, so the status is returned through the Unsupported path only when
        // the reason says so. Anything else is a plain failure with the same text.
        if (why.find("only Motion JPEG") != std::string::npos ||
            why.find("muxed into AVI") != std::string::npos) {
            return fail(RecorderStatusUnsupported, "%s", why.c_str());
        }
        return fail(RecorderStatusError, "%s", why.c_str());
    }
    *out = session;
    return kOk;
}

int32_t encoderEncode(EncoderSession* encoder, const uint8_t* bgra, int32_t stride,
                      uint64_t duration100ns) {
    if (encoder == nullptr) {
        return fail(RecorderStatusError, "encode: null encoder");
    }
    return encoder->encode(bgra, stride, duration100ns);
}

int32_t encoderAudioWrite(EncoderSession* encoder, const uint8_t* data, uint32_t size) {
    if (encoder == nullptr) {
        return fail(RecorderStatusError, "audio_write: null encoder");
    }
    if (data == nullptr) {
        return fail(RecorderStatusError, "audio_write: null argument");
    }
    return encoder->audioWrite(data, size);
}

int32_t encoderRead(EncoderSession* encoder, RecorderBuffer* out, uint32_t capacity) {
    if (encoder == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "encoder_read: null argument");
    }
    (void)capacity;
    return 0;  // streaming: chunks go straight to disk, nothing is queued
}

int32_t encoderFlush(EncoderSession* encoder) {
    return encoder == nullptr ? kOk : encoder->flush();
}

int32_t encoderFinish(EncoderSession* encoder, const char* path) {
    if (encoder == nullptr) {
        return fail(RecorderStatusError, "finish: null encoder");
    }
    return encoder->finish(path);
}

}  // namespace recorder
