// Adapter discovery: what displays exist, what is behind them, and whether the
// machine is worth using a GPU for.
//
// The recorder must work on a machine with no usable graphics card, so every
// query here answers with a fact rather than an error: an empty list, a zero
// count, or the software path is a valid answer, not a failure.
#include <d3d11.h>
#include <dxgi1_2.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mmdeviceapi.h>
#include <wincodec.h>

// Media Foundation encoder enumeration flags. An older revision of this file
// spelled them by hand and got them wrong (0x2 is ASYNCMFT, not HARDWARE),
// which reported async encoders as hardware ones; the SDK names are used so
// the meaning stays attached to the value.
#ifndef MFT_ENUM_FLAG_HARDWARE
#define MFT_ENUM_FLAG_HARDWARE 0x00000004
#endif
#ifndef MFT_ENUM_FLAG_SORTANDFILTER
#define MFT_ENUM_FLAG_SORTANDFILTER 0x00000040
#endif

#include <algorithm>
#include <cstring>
#include <vector>

#include "common.h"

namespace recorder {

constexpr uint32_t kMaxAdapters = 16;
constexpr uint32_t kMaxOutputsPerAdapter = 16;

// softwareAdapter reports whether an adapter is a software rasteriser, which
// means it can decode and display but cannot encode video at any useful speed.
bool isSoftwareAdapter(IDXGIAdapter1* adapter) {
    DXGI_ADAPTER_DESC1 desc{};
    if (FAILED(adapter->GetDesc1(&desc))) {
        return false;
    }
    if ((desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) != 0) {
        return true;
    }
    // 0x1414 is Microsoft's basic display adapter and 0x8B40 the WARP device;
    // both enumerate as adapters while having no encoder behind them.
    return desc.VendorId == 0x1414 || desc.VendorId == 0x8B40;
}

// fillOutput gathers everything the interface needs to describe one output.
void fillOutput(IDXGIAdapter1* adapter, uint32_t adapterIndex, uint32_t outputIndex,
                RecorderAdapterInfo* info) {
    ComPtr<IDXGIOutput> output;
    if (FAILED(adapter->EnumOutputs(outputIndex, output.put()))) {
        return;
    }

    DXGI_OUTPUT_DESC outputDesc{};
    if (FAILED(output->GetDesc(&outputDesc))) {
        return;
    }

    info->adapter_index = adapterIndex;
    info->output_index = outputIndex;
    info->bounds.left = outputDesc.DesktopCoordinates.left;
    info->bounds.top = outputDesc.DesktopCoordinates.top;
    info->bounds.right = outputDesc.DesktopCoordinates.right;
    info->bounds.bottom = outputDesc.DesktopCoordinates.bottom;
    // DXGI_MODE_ROTATION counts identity as 1 (0 is unspecified, 2 is 90°),
    // while the ABI counts quarter-turns from an upright panel, so the value
    // is translated rather than copied: an unrotated display reports 0.
    switch (outputDesc.Rotation) {
        case 2:
            info->rotation = 1;
            break;
        case 3:
            info->rotation = 2;
            break;
        case 4:
            info->rotation = 3;
            break;
        default:
            info->rotation = 0;
            break;
    }
    info->attached_to_desktop = outputDesc.AttachedToDesktop != 0 ? 1u : 0u;
    info->primary = (outputDesc.DesktopCoordinates.left == 0 &&
                     outputDesc.DesktopCoordinates.top == 0)
                        ? 1u
                        : 0u;

    DXGI_ADAPTER_DESC1 adapterDesc{};
    if (SUCCEEDED(adapter->GetDesc1(&adapterDesc))) {
        info->vendor_id = adapterDesc.VendorId;
        info->device_id = adapterDesc.DeviceId;
        info->is_software = isSoftwareAdapter(adapter) ? 1u : 0u;
        copyString(info->description, RECORDER_ADAPTER_DESC_CAP,
                   toUtf8(adapterDesc.Description));
    } else {
        info->is_software = 1u;
    }
    copyString(info->device_name, RECORDER_ADAPTER_NAME_CAP, toUtf8(outputDesc.DeviceName));

    // The refresh rate is asked of the Win32 display settings for this output's
    // device name. DXGI's mode list is awkward to query per output, and the
    // answer here is a presentation detail rather than something the capture
    // path depends on, so a fallback of 60 Hz is acceptable.
    DEVMODEW mode{};
    mode.dmSize = sizeof(mode);
    if (EnumDisplaySettingsW(outputDesc.DeviceName, ENUM_CURRENT_SETTINGS, &mode) != 0 &&
        mode.dmDisplayFrequency > 0) {
        info->refresh_hz = static_cast<float>(mode.dmDisplayFrequency);
    } else {
        info->refresh_hz = 60.0f;
    }
}

// deviceOnAdapter creates a D3D11 device that belongs to the given adapter.
//
// Passing the adapter to D3D11CreateDevice is the correct request but some
// installations reject it outright, in which case the default adapter is the
// only device available. That is only good enough when the default adapter *is*
// the one driving the output, so the adapter the device reports is compared
// against the adapter we asked about rather than assumed.
ComPtr<ID3D11Device> deviceOnAdapter(IDXGIAdapter1* adapter) {
    static const D3D_FEATURE_LEVEL levels[] = {
        D3D_FEATURE_LEVEL_12_1, D3D_FEATURE_LEVEL_12_0, D3D_FEATURE_LEVEL_11_1,
        D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1, D3D_FEATURE_LEVEL_10_0,
        D3D_FEATURE_LEVEL_9_3,
    };

    DXGI_ADAPTER_DESC1 wanted{};
    const bool haveLuid = SUCCEEDED(adapter->GetDesc1(&wanted));

    for (int pass = 0; pass < 2; ++pass) {
        ComPtr<ID3D11Device> device;
        D3D_FEATURE_LEVEL obtained = D3D_FEATURE_LEVEL_9_1;
        // The feature level array is walked down from the most capable entry
        // because D3D11CreateDevice rejects the whole call, with E_INVALIDARG,
        // when it names a level the installed runtime does not know.
        for (uint32_t skip = 0; skip < ARRAYSIZE(levels); ++skip) {
            HRESULT hr;
            if (pass == 0) {
                hr = D3D11CreateDevice(adapter, D3D_DRIVER_TYPE_UNKNOWN, nullptr, 0,
                                       levels + skip, ARRAYSIZE(levels) - skip,
                                       D3D11_SDK_VERSION, device.put(), &obtained, nullptr);
            } else {
                hr = D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr, 0,
                                       levels + skip, ARRAYSIZE(levels) - skip,
                                       D3D11_SDK_VERSION, device.put(), &obtained, nullptr);
            }
            if (SUCCEEDED(hr)) {
                break;
            }
            if (hr != E_INVALIDARG) {
                device.reset();
                break;
            }
        }
        if (!device) {
            continue;
        }
        if (pass == 0) {
            return device;
        }

        // On the fallback pass the device may be on a different adapter, which
        // duplication would reject with E_INVALIDARG later.
        if (!haveLuid) {
            return device;
        }
        ComPtr<IDXGIDevice> dxgiDevice;
        ComPtr<IDXGIAdapter> plainAdapter;
        ComPtr<IDXGIAdapter1> deviceAdapter;
        if (queryInterface(device.get(), &dxgiDevice) &&
            SUCCEEDED(dxgiDevice->GetAdapter(plainAdapter.put())) &&
            queryInterface(plainAdapter.get(), &deviceAdapter)) {
            DXGI_ADAPTER_DESC1 got{};
            if (SUCCEEDED(deviceAdapter->GetDesc1(&got)) &&
                got.AdapterLuid.LowPart == wanted.AdapterLuid.LowPart &&
                got.AdapterLuid.HighPart == wanted.AdapterLuid.HighPart) {
                return device;
            }
        }
    }
    return ComPtr<ID3D11Device>();
}

// canDuplicate reports whether duplication is accepted for an output.
bool canDuplicate(IDXGIAdapter1* adapter, uint32_t outputIndex) {
    ComPtr<IDXGIOutput> output;
    if (FAILED(adapter->EnumOutputs(outputIndex, output.put()))) {
        return false;
    }
    ComPtr<IDXGIOutput1> output1;
    if (!queryInterface(output.get(), &output1)) {
        return false;
    }
    ComPtr<ID3D11Device> device = deviceOnAdapter(adapter);
    if (!device) {
        return false;
    }
    ComPtr<IDXGIOutputDuplication> duplication;
    return SUCCEEDED(output1->DuplicateOutput(device.get(), duplication.put()));
}

// enumerateAdapters is the shared implementation behind both the list and the
// capability summary, so the two can never disagree.
std::vector<RecorderAdapterInfo> enumerateAdapters() {
    std::vector<RecorderAdapterInfo> results;
    ensureCom();
    ComPtr<IDXGIFactory1> factory;
    HRESULT hr = CreateDXGIFactory1(__uuidof(IDXGIFactory1),
                                    reinterpret_cast<void**>(factory.put()));
    if (FAILED(hr)) {
        mutableLastError().clear();
        return results;
    }

    for (uint32_t ai = 0; ai < kMaxAdapters; ++ai) {
        ComPtr<IDXGIAdapter1> adapter;
        if (FAILED(factory->EnumAdapters1(ai, adapter.put()))) {
            break;
        }
        for (uint32_t oi = 0; oi < kMaxOutputsPerAdapter; ++oi) {
            ComPtr<IDXGIOutput> probe;
            if (FAILED(adapter->EnumOutputs(oi, probe.put()))) {
                break;
            }
            DXGI_OUTPUT_DESC desc{};
            if (FAILED(probe->GetDesc(&desc)) ||
                desc.DesktopCoordinates.right <= desc.DesktopCoordinates.left) {
                break;
            }

            RecorderAdapterInfo info{};
            info.struct_size = sizeof(RecorderAdapterInfo);
            fillOutput(adapter.get(), ai, oi, &info);
            info.can_duplicate = canDuplicate(adapter.get(), oi) ? 1u : 0u;
            results.push_back(info);
        }
    }

    // Left to right, top to bottom, so the first entry is the leftmost display
    // rather than whatever the driver happened to enumerate first.
    std::stable_sort(results.begin(), results.end(),
                     [](const RecorderAdapterInfo& a, const RecorderAdapterInfo& b) {
                         if (a.bounds.left != b.bounds.left) {
                             return a.bounds.left < b.bounds.left;
                         }
                         return a.bounds.top < b.bounds.top;
                     });
    return results;
}

uint32_t enumerateAdapters(RecorderAdapterInfo* out, uint32_t capacity) {
    const std::vector<RecorderAdapterInfo> list = enumerateAdapters();
    const uint32_t count = static_cast<uint32_t>(list.size());
    if (out != nullptr) {
        const uint32_t copy = count < capacity ? count : capacity;
        memcpy(out, list.data(), sizeof(RecorderAdapterInfo) * copy);
        if (copy < count) {
            // Tell the caller how much it missed rather than truncating silently.
            mutableLastError() = "adapter buffer too small";
        }
    }
    return count;
}

// wicJpegAvailable reports whether the WIC JPEG encoder exists, which is the
// only encoder the MJPEG path needs. It is an inbox component since Vista, so
// this is true on any machine the recorder runs on; it is still asked rather
// than assumed so the diagnostics report stays honest.
bool wicJpegAvailable() {
    ComScope com;
    ComPtr<IWICImagingFactory> factory;
    if (FAILED(CoCreateInstance(CLSID_WICImagingFactory, nullptr, CLSCTX_INPROC_SERVER,
                                __uuidof(IWICImagingFactory),
                                reinterpret_cast<void**>(factory.put())))) {
        return false;
    }
    ComPtr<IWICBitmapEncoder> encoder;
    return SUCCEEDED(factory->CreateEncoder(GUID_ContainerFormatJpeg, nullptr,
                                           encoder.put()));
}

void fillCapabilities(RecorderCapabilities* out) {
    memset(out, 0, sizeof(*out));
    out->struct_size = sizeof(*out);

    const std::vector<RecorderAdapterInfo> list = enumerateAdapters();
    for (const RecorderAdapterInfo& info : list) {
        out->adapter_count++;
        if (!info.is_software) {
            out->adapter_count_hardware++;
        }
        if (info.can_duplicate) {
            out->has_dxgi_duplication = 1;
        }
        if (info.primary && info.description[0] != '\0') {
            copyString(out->renderer, RECORDER_ADAPTER_DESC_CAP, info.description);
        }
    }
    if (out->renderer[0] == '\0' && !list.empty()) {
        copyString(out->renderer, RECORDER_ADAPTER_DESC_CAP, list.front().description);
    }

    // H.264 encoders are enumerated rather than assumed: a GPU with no encode
    // block still records through the inbox software encoder, and a machine
    // with neither falls back to MJPEG instead of failing. H.265 stays
    // reserved and reports as absent.
    out->hardware_h264 = 0;
    out->software_h264 = 0;
    out->hardware_h265 = 0;
    out->software_h265 = 0;
    {
        ensureCom();
        const HRESULT started = MFStartup(MF_VERSION, MFSTARTUP_LITE);
        if (SUCCEEDED(started) || started == E_ACCESSDENIED) {
            // A partial output description restricts the enumeration to H.264
            // encoders; without it any video encoder on the box would count.
            // There is no software-only flag, so software is whatever is left
            // after the hardware encoders are counted.
            const MFT_REGISTER_TYPE_INFO wantH264 = {MFMediaType_Video, MFVideoFormat_H264};
            UINT32 hardwareCount = 0;
            UINT32 allCount = 0;
            for (int pass = 0; pass < 2; ++pass) {
                IMFActivate** found = nullptr;
                UINT32 count = 0;
                const UINT32 flags =
                    pass == 0 ? (MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER)
                              : MFT_ENUM_FLAG_SORTANDFILTER;
                const HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, flags, nullptr,
                                             &wantH264, &found, &count);
                if (SUCCEEDED(hr)) {
                    if (pass == 0) {
                        hardwareCount = count;
                    } else {
                        allCount = count;
                    }
                }
                if (found != nullptr) {
                    CoTaskMemFree(found);
                }
            }
            out->hardware_h264 = hardwareCount > 0 ? 1u : 0u;
            out->software_h264 = allCount > hardwareCount ? 1u : 0u;
        }
    }
    out->mjpeg = wicJpegAvailable() ? 1u : 0u;

    // Audio is a separate fact from video: a machine can record a silent
    // recording perfectly well, so this is reported rather than required.
    ComPtr<IMMDeviceEnumerator> enumerator;
    if (SUCCEEDED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr,
                                   CLSCTX_INPROC_SERVER, __uuidof(IMMDeviceEnumerator),
                                   reinterpret_cast<void**>(enumerator.put())))) {
        ComPtr<IMMDevice> device;
        out->audio_loopback =
            SUCCEEDED(enumerator->GetDefaultAudioEndpoint(eRender, eConsole, device.put()))
                ? 1u
                : 0u;
    }
}

}  // namespace recorder
