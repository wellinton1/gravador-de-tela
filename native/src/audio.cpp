// Audio capture: WASAPI loopback on the default render endpoint.
//
// Loopback is what a screen recorder wants, because it records what the speakers
// play rather than what the microphone hears. A machine with no render endpoint
// has no sound to record, which is a fact to report rather than a failure.
#include "internal.h"

#include <audioclient.h>
#include <mmdeviceapi.h>
#include <propsys.h>
#include <propidl.h>
#include <functiondiscoverykeys_devpkey.h>

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <new>
#include <vector>

namespace recorder {
namespace {

constexpr uint32_t kFrameCapacity = 48000; // one second of frames at 48 kHz

// The SDK's capture client reports the buffer state through the flags pointer of
// GetBuffer and takes the frame count alone in ReleaseBuffer, so there is no
// flags argument to pass back.
constexpr HRESULT kBufferEmpty = MAKE_HRESULT(0x001, FACILITY_AUDIO, 0x020);

// loopbackCapture reads the mix the system is playing.
class LoopbackCapture final : public AudioSession {
public:
    ~LoopbackCapture() override { close(); }

    bool open() {
        // The client outlives this call, so COM must stay up: see ensureCom.
        ensureCom();

        ComPtr<IMMDeviceEnumerator> enumerator;
        HRESULT hr = CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr,
                                      CLSCTX_INPROC_SERVER, __uuidof(IMMDeviceEnumerator),
                                      reinterpret_cast<void**>(enumerator.put()));
        if (FAILED(hr)) {
            mutableLastError() = "the audio device enumerator is unavailable";
            return false;
        }

        // Candidate endpoints in order: the console-role default, the
        // multimedia-role default, then every active render endpoint. The
        // default is sometimes a powered-off headset or a role with nothing
        // behind it; stopping at the first refusal is what made loopback
        // "unavailable" on machines that had a working endpoint all along.
        // Each entry is tried until one initialises; the last refusal is
        // what gets reported when none do.
        std::string lastWhy = "no audio output device is available";
        const ERole roles[] = {eConsole, eMultimedia};
        for (ERole role : roles) {
            ComPtr<IMMDevice> device;
            if (FAILED(enumerator->GetDefaultAudioEndpoint(eRender, role, device.put())) ||
                !device) {
                continue;
            }
            if (openOnDevice(device.get(), &lastWhy)) {
                return true;
            }
        }
        ComPtr<IMMDeviceCollection> collection;
        if (SUCCEEDED(enumerator->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE,
                                                     collection.put())) &&
            collection) {
            UINT32 count = 0;
            if (SUCCEEDED(collection->GetCount(&count))) {
                for (UINT32 i = 0; i < count; ++i) {
                    ComPtr<IMMDevice> device;
                    if (FAILED(collection->Item(i, device.put())) || !device) {
                        continue;
                    }
                    if (openOnDevice(device.get(), &lastWhy)) {
                        return true;
                    }
                }
            }
        }

        mutableLastError() = lastWhy;
        return false;
    }

    // openOnDevice initialises loopback on one endpoint, trying the lenient
    // request first and the explicit one second. A null format with a zero
    // period works on most desktops, but finicky (often USB or wireless)
    // endpoints refuse it and only accept their own mix format with the
    // engine's default period — including the 0x80070023 some of them answer
    // the lenient request with. Each attempt needs a fresh client: a failed
    // Initialize leaves the one it ran on unusable.
    bool openOnDevice(IMMDevice* device, std::string* lastWhy) {
        for (int attempt = 0; attempt < 2; ++attempt) {
            ComPtr<IAudioClient> client;
            HRESULT hr = device->Activate(__uuidof(IAudioClient), CLSCTX_INPROC_SERVER, nullptr,
                                          reinterpret_cast<void**>(client.put()));
            if (FAILED(hr) || !client) {
                noteFailure(lastWhy, hr, "the audio device could not be opened for capture");
                continue;
            }

            WAVEFORMATEX* mix = nullptr;
            REFERENCE_TIME period = 0;
            if (attempt == 1) {
                if (FAILED(client->GetMixFormat(&mix)) || mix == nullptr) {
                    noteFailure(lastWhy, E_FAIL, "the audio device did not report its mix format");
                    continue;
                }
                REFERENCE_TIME minPeriod = 0;
                if (FAILED(client->GetDevicePeriod(&period, &minPeriod))) {
                    period = 0;
                }
                (void)minPeriod;
            }
            hr = client->Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK,
                                    period, 0, mix, nullptr);
            if (mix != nullptr) {
                CoTaskMemFree(mix);
            }
            if (FAILED(hr)) {
                noteFailure(lastWhy, hr, "the audio device refused loopback capture");
                continue;
            }

            if (startClient(client)) {
                return true;
            }
            // startClient recorded why.
            *lastWhy = lastError();
        }
        return false;
    }

    void noteFailure(std::string* lastWhy, HRESULT hr, const char* what) {
        char code[16];
        snprintf(code, sizeof(code), "%08lX", static_cast<unsigned long>(hr));
        *lastWhy = std::string(what) + " (0x" + code + ")";
    }

    bool startClient(ComPtr<IAudioClient>& client) {
        WAVEFORMATEX* mix = nullptr;
        HRESULT hr = client->GetMixFormat(&mix);
        if (FAILED(hr) || mix == nullptr) {
            mutableLastError() = "the audio device did not report its mix format";
            return false;
        }
        // The whole descriptor is kept, extension included, so the muxer
        // sees the exact layout the mix arrives in. The SubFormat GUID rides
        // past the base descriptor for extensible mixes; its Data1 (at base
        // + 24) is the plain tag this file reports downstream.
        subTag_ = 0;
        if (mix->wFormatTag == 0xFFFE /* EXTENSIBLE */ && mix->cbSize >= 22) {
            uint32_t sub = 0;
            memcpy(&sub, reinterpret_cast<const uint8_t*>(mix) + sizeof(WAVEFORMATEX) + 6,
                   sizeof(sub));
            if (sub == 1 || sub == 3) {
                subTag_ = sub;
            }
        }
        format_ = *mix;
        CoTaskMemFree(mix);
        if (format_.nBlockAlign == 0) {
            format_.nBlockAlign =
                static_cast<WORD>(format_.wBitsPerSample / 8 * format_.nChannels);
        }
        if (format_.nChannels == 0 || format_.nSamplesPerSec == 0) {
            mutableLastError() = "the audio device reported an unusable format";
            return false;
        }

        // The buffer itself is reached through the capture service; on the
        // IAudioClient interface there is only the size and the padding.
        if (FAILED(client->GetService(IID_IAudioCaptureClient,
                                     reinterpret_cast<void**>(capture_.put())))) {
            mutableLastError() = "the audio device offers no capture buffer";
            return false;
        }

        hr = client->GetBufferSize(&bufferFrames_);
        if (FAILED(hr)) {
            mutableLastError() = "the audio client has no buffer";
            return false;
        }
        client_.attach(client.detach());

        // The capture is started in one go; a recorder wants everything that has
        // already been played, so the client is reset before the first read.
        client_->Reset();
        hr = client_->Start();
        if (FAILED(hr)) {
            mutableLastError() = "audio capture could not be started";
            return false;
        }
        running_ = true;
        return true;
    }

    int32_t read(uint8_t* buffer, uint32_t capacity, int32_t* frames) override {
        if (!running_ || !client_) {
            return fail(RecorderStatusError, "audio: the capture is closed");
        }
        if (buffer == nullptr || frames == nullptr) {
            return fail(RecorderStatusError, "audio_read: null argument");
        }

        const UINT32 bytesPerFrame =
            format_.nBlockAlign != 0
                ? format_.nBlockAlign
                : format_.wBitsPerSample / 8 * format_.nChannels;

        UINT32 available = 0;
        HRESULT hr = capture_->GetNextPacketSize(&available);
        if (hr == AUDCLNT_E_DEVICE_INVALIDATED || hr == AUDCLNT_E_SERVICE_NOT_RUNNING) {
            return fail(RecorderStatusLost, "the audio device disappeared (0x%08lx)", hr);
        }
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "the audio client failed (0x%08lx)", hr);
        }

        const UINT32 wantedFrames =
            std::min<UINT32>(available, std::min<UINT32>(capacity / bytesPerFrame, kFrameCapacity));
        if (wantedFrames == 0) {
            *frames = 0;
            return RecorderStatusTimeout;
        }

        BYTE* data = nullptr;
        UINT32 contiguousFrames = 0;
        DWORD flags = 0;
        hr = capture_->GetBuffer(&data, &contiguousFrames, &flags, nullptr, nullptr);
        if (hr == kBufferEmpty) {
            *frames = 0;
            return RecorderStatusTimeout;
        }
        if (FAILED(hr)) {
            return fail(RecorderStatusError, "the audio buffer could not be read (0x%08lx)", hr);
        }

        const UINT32 bytes = contiguousFrames * bytesPerFrame;
        memcpy(buffer, data, std::min(bytes, capacity));
        capture_->ReleaseBuffer(contiguousFrames);
        *frames = static_cast<int32_t>(contiguousFrames);
        return kOk;
    }

    void format(RecorderAudioFormat* out) const override {
        memset(out, 0, sizeof(*out));
        out->struct_size = sizeof(*out);
        out->sample_rate = format_.nSamplesPerSec;
        out->channels = static_cast<uint32_t>(format_.nChannels);
        out->bits_per_sample = format_.wBitsPerSample;
        // WASAPI usually reports EXTENSIBLE (0xFFFE), which containers cannot
        // use directly: the real layout hides in the SubFormat GUID that
        // follows the base descriptor, with the plain wave tag in its first
        // DWORD (1 = PCM, 3 = IEEE float). Resolving it here keeps every
        // consumer — AVI, MP4, AAC — on plain tags with no ABI change.
        out->format_tag = subTag_ != 0 ? subTag_ : format_.wFormatTag;
    }

private:
    void close() {
        if (client_ && running_) {
            client_->Stop();
            running_ = false;
        }
        capture_ = ComPtr<IAudioCaptureClient>();
        client_ = ComPtr<IAudioClient>();
    }

    ComPtr<IAudioClient> client_;
    ComPtr<IAudioCaptureClient> capture_;
    WAVEFORMATEX format_{};
    uint32_t subTag_ = 0;  // resolved plain tag for EXTENSIBLE mixes
    UINT32 bufferFrames_ = 0;
    bool running_ = false;
};

}  // namespace

int32_t audioOpen(const RecorderAudioConfig* config, AudioSession** out) {
    if (config == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "audio_open: null argument");
    }
    *out = nullptr;
    auto* audio = new (std::nothrow) LoopbackCapture();
    if (audio == nullptr) {
        return fail(RecorderStatusError, "audio_open: out of memory");
    }
    if (!audio->open()) {
        const std::string why = lastError();
        delete audio;
        return fail(RecorderStatusUnsupported, "%s", why.c_str());
    }
    *out = audio;
    return kOk;
}

int32_t audioRead(AudioSession* audio, uint8_t* buffer, uint32_t capacity, int32_t* frames) {
    if (audio == nullptr) {
        return fail(RecorderStatusError, "audio_read: null capture");
    }
    return audio->read(buffer, capacity, frames);
}

int32_t audioFormat(AudioSession* audio, RecorderAudioFormat* out) {
    if (audio == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "audio_format: null argument");
    }
    audio->format(out);
    return kOk;
}

uint32_t audioCount(char** names, uint32_t capacity) {
    ensureCom();
    ComPtr<IMMDeviceEnumerator> enumerator;
    if (FAILED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_INPROC_SERVER,
                                __uuidof(IMMDeviceEnumerator),
                                reinterpret_cast<void**>(enumerator.put())))) {
        return 0;
    }
    ComPtr<IMMDeviceCollection> collection;
    if (FAILED(enumerator->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE,
                                             collection.put()))) {
        return 0;
    }
    UINT32 count = 0;
    if (!collection || FAILED(collection->GetCount(&count)) || count == 0) {
        return 0;
    }

    uint32_t written = 0;
    for (UINT32 i = 0; i < count; ++i) {
        ComPtr<IMMDevice> device;
        if (FAILED(collection->Item(i, device.put()))) {
            continue;
        }
        ComPtr<IPropertyStore> properties;
        if (FAILED(device->OpenPropertyStore(STGM_READ, properties.put()))) {
            continue;
        }
        PROPVARIANT name{};
        PropVariantInit(&name);
        if (SUCCEEDED(properties->GetValue(PKEY_Device_FriendlyName, &name)) &&
            name.vt == VT_LPWSTR && name.pwszVal != nullptr) {
            if (names != nullptr && written < capacity) {
                recorder::copyString(names[written], 128, recorder::toUtf8(name.pwszVal));
            }
            ++written;
        }
        PropVariantClear(&name);
    }
    return written;
}

}  // namespace recorder
