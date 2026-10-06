// Screen capture.
//
// Two backends implement one interface. DXGI desktop duplication is preferred
// because it returns the composited desktop as a GPU texture, already including
// the hardware cursor. GDI BitBlt is the fallback for machines where duplication
// is refused, which includes virtualised desktops and remote sessions.
//
// The caller never learns which backend it got beyond the reported value, and the
// recorder keeps working either way, because a machine without a usable graphics
// path must still record.
#include "internal.h"

#include <stdarg.h>
#include <cstdlib>

#include <algorithm>
#include <cstring>
#include <new>

namespace recorder {
namespace {

// Desktop duplication reports "nothing new arrived" and "the output is gone"
// with these two codes.
constexpr HRESULT kNoNewFrame = static_cast<HRESULT>(0x887A0027L);
constexpr HRESULT kAccessLost = static_cast<HRESULT>(0x887A0026L);

// Geometry tracing, off unless asked for: the relationship between the desktop
// rectangle, the duplication image and the panel orientation is not visible from
// the outside, and getting it wrong is the difference between a picture and a
// grey rectangle.
bool traceCapture() {
    static const bool on = std::getenv("SR_CAPTURE_TRACE") != nullptr;
    return on;
}

// blendPixel mixes a BGR colour into a BGRA pixel at the given opacity,
// leaving the alpha channel alone: the session buffers are opaque.
inline void blendPixel(uint8_t* px, uint8_t b, uint8_t g, uint8_t r, uint32_t alpha) {
    px[0] = static_cast<uint8_t>((px[0] * (255 - alpha) + b * alpha) / 255);
    px[1] = static_cast<uint8_t>((px[1] * (255 - alpha) + g * alpha) / 255);
    px[2] = static_cast<uint8_t>((px[2] * (255 - alpha) + r * alpha) / 255);
}

// paintOverlays draws the halo and/or click ring at frame coordinates. Both
// backends call it after the pointer position is known, so the compositor
// cursor and the decoration never disagree about where the pointer is.
void paintOverlays(uint8_t* pixels, int stride, int width, int height, int cx, int cy,
                   bool halo, bool click) {
    if (halo) {
        constexpr int kRadius = 44;
        for (int y = cy - kRadius; y <= cy + kRadius; ++y) {
            if (y < 0 || y >= height) {
                continue;
            }
            for (int x = cx - kRadius; x <= cx + kRadius; ++x) {
                if (x < 0 || x >= width) {
                    continue;
                }
                const int dx = x - cx;
                const int dy = y - cy;
                if (dx * dx + dy * dy <= kRadius * kRadius) {
                    blendPixel(pixels + static_cast<size_t>(y) * stride + x * 4, 0, 255,
                               255, 42);
                }
            }
        }
    }
    if (click) {
        constexpr int kRadius = 20;
        constexpr int kThick = 3;
        for (int y = cy - kRadius - kThick; y <= cy + kRadius + kThick; ++y) {
            if (y < 0 || y >= height) {
                continue;
            }
            for (int x = cx - kRadius - kThick; x <= cx + kRadius + kThick; ++x) {
                if (x < 0 || x >= width) {
                    continue;
                }
                const int dx = x - cx;
                const int dy = y - cy;
                const int d2 = dx * dx + dy * dy;
                const int inner = (kRadius - kThick) * (kRadius - kThick);
                const int outer = (kRadius + kThick) * (kRadius + kThick);
                if (d2 >= inner && d2 <= outer) {
                    blendPixel(pixels + static_cast<size_t>(y) * stride + x * 4, 32, 32,
                               235, 230);
                }
            }
        }
    }
}

// One output being duplicated, with the staging texture used to read it back.
struct DuplicationOutput {
    ComPtr<IDXGIOutputDuplication> duplication;
    ComPtr<ID3D11Texture2D> staging;
    UINT width = 0;
    UINT height = 0;
    DXGI_OUTDUPL_DESC desc{};
};

class DxgiCapture final : public CaptureSession {
public:
    // Thread affinity is the caller's responsibility and is documented in the
    // header: the session must be driven from one pinned thread, which is what
    // Go's runtime.LockOSThread gives us.
    DxgiCapture() = default;
    ~DxgiCapture() override = default;

    bool open(uint32_t adapterIndex, uint32_t outputIndex, const RecorderRect& region,
              bool hasRegion, bool trackCursor, bool hideCursorOnClick, uint32_t flags) {
        showClick_ = (flags & RecorderCaptureFlagClickRing) != 0;
        halo_ = (flags & RecorderCaptureFlagHalo) != 0;
        // The device outlives this call, so COM must stay up: see ensureCom.
        ensureCom();
        ComPtr<IDXGIFactory1> factory;
        HRESULT hr = CreateDXGIFactory1(__uuidof(IDXGIFactory1),
                                        reinterpret_cast<void**>(factory.put()));
        if (FAILED(hr)) {
            return report("CreateDXGIFactory1 failed (0x%08lx)", hr);
        }

        ComPtr<IDXGIAdapter1> adapter;
        if (FAILED(factory->EnumAdapters1(adapterIndex, adapter.put()))) {
            return report("adapter %u does not exist", adapterIndex);
        }
        ComPtr<IDXGIOutput> output;
        if (FAILED(adapter->EnumOutputs(outputIndex, output.put()))) {
            return report("output %u does not exist on adapter %u", outputIndex, adapterIndex);
        }
        DXGI_OUTPUT_DESC outputDesc{};
        if (FAILED(output->GetDesc(&outputDesc))) {
            return report("IDXGIOutput::GetDesc failed");
        }
        ComPtr<IDXGIOutput1> output1;
        if (!queryInterface(output.get(), &output1)) {
            return report("the output does not support IDXGIOutput1");
        }

        device_ = deviceOnAdapter(adapter.get());
        if (!device_) {
            return report("no D3D11 device could be created for this display");
        }
        device_->GetImmediateContext(context_.put());
        if (!context_) {
            return report("the graphics device produced no immediate context");
        }

        DuplicationOutput state;
        if (FAILED(output1->DuplicateOutput(device_.get(), state.duplication.put()))) {
            return report("IDXGIOutput1::DuplicateOutput was refused by this machine");
        }
        state.duplication->GetDesc(&state.desc);
        if (state.desc.ModeDesc.Width == 0 || state.desc.ModeDesc.Height == 0) {
            return report("the duplication reported no desktop size");
        }

        bounds_ = RecorderRect{outputDesc.DesktopCoordinates.left,
                               outputDesc.DesktopCoordinates.top,
                               outputDesc.DesktopCoordinates.right,
                               outputDesc.DesktopCoordinates.bottom};
        rotation_ = static_cast<uint32_t>(outputDesc.Rotation);
        if (hasRegion) {
            const RecorderRect clipped = rectIntersect(bounds_, region);
            if (rectEmpty(clipped)) {
                return report("the selected region is outside this display");
            }
            bounds_ = clipped;
        }

        const int width = rectWidth(bounds_);
        const int height = rectHeight(bounds_);
        if (width <= 0 || height <= 0) {
            return report("the captured region has no area");
        }
        stride_ = alignUp(width * 4, 64);
        pixels_.assign(static_cast<size_t>(stride_) * height, 0);
        outputs_.push_back(std::move(state));

        trackCursor_ = trackCursor;
        hideCursorOnClick_ = hideCursorOnClick;
        return true;
    }

    int32_t grab(int32_t timeoutMs) override {
        if (timeoutMs < 1) {
            timeoutMs = 1;
        }
        const DWORD timeout = static_cast<DWORD>(timeoutMs);

        int acquired = 0;
        HRESULT firstFailure = S_OK;
        for (DuplicationOutput& state : outputs_) {
            DXGI_OUTDUPL_FRAME_INFO frameInfo{};
            ComPtr<IDXGIResource> resource;
            HRESULT hr = state.duplication->AcquireNextFrame(timeout, &frameInfo,
                                                             resource.put());
            if (hr == kNoNewFrame) {
                continue;
            }
            if (FAILED(hr)) {
                firstFailure = hr;
                continue;
            }
            if (copyOutput(state, resource.get(), frameInfo)) {
                acquired++;
                notePointer(frameInfo);
            }
            state.duplication->ReleaseFrame();
        }

        if (acquired == 0) {
            if (firstFailure != S_OK && firstFailure != kNoNewFrame) {
                return fail(RecorderStatusError,
                            "desktop duplication failed (0x%08lx); the display mode may "
                            "have changed",
                            firstFailure);
            }
            // No new frame is not a failure: a static desktop produces none.
            return RecorderStatusTimeout;
        }
        if (trackCursor_) {
            detectCursor();
        }
        // Overlays bake into the same pixels the encoder reads right after,
        // so they cost one pass over a small disc, not a second capture.
        if ((showClick_ || halo_) && pointerKnown_ && pointerVisible_) {
            const bool clicked = showClick_ && primaryMouseDown();
            if (halo_ || clicked) {
                paintOverlays(pixels_.data(), stride_, rectWidth(bounds_), rectHeight(bounds_),
                              pointerLeft_ - bounds_.left, pointerTop_ - bounds_.top, halo_,
                              clicked);
            }
        }
        return kOk;
    }

    void frame(RecorderFrame* out) const override {
        memset(out, 0, sizeof(*out));
        out->struct_size = sizeof(*out);
        out->width = static_cast<uint32_t>(rectWidth(bounds_));
        out->height = static_cast<uint32_t>(rectHeight(bounds_));
        out->stride = stride_;
        out->present_ticks = presentTicks_;
        out->accumulated_frames = accumulatedFrames_;
        out->cursor = cursor_;
    }

    void cursor(RecorderCursor* out) const override {
        *out = cursor_;
    }

    uint32_t backend() const override { return RecorderBackendDxgi; }

    const uint8_t* pixels() const override { return pixels_.data(); }

private:
    bool report(const char* format, ...) {
        char buffer[512];
        va_list args;
        va_start(args, format);
        vsnprintf(buffer, sizeof(buffer), format, args);
        va_end(args);
        mutableLastError() = buffer;
        return false;
    }

    // copyOutput moves one acquired frame into the session buffer, rotating it
    // upright and clipping it to the captured region.
    bool copyOutput(DuplicationOutput& state, IDXGIResource* resource,
                    const DXGI_OUTDUPL_FRAME_INFO& frameInfo) {
        // AcquireNextFrame hands back an IDXGIResource; the read-back needs the
        // D3D11 view of it, which sits behind a different vtable.
        ComPtr<ID3D11Texture2D> texture;
        if (!queryInterface(resource, &texture)) {
            mutableLastError() = "the acquired desktop image is not a D3D11 texture";
            return false;
        }

        D3D11_TEXTURE2D_DESC desc{};
        texture->GetDesc(&desc);
        if (desc.Width == 0 || desc.Height == 0) {
            mutableLastError() = "the acquired desktop frame reported no size";
            return false;
        }

        if (!state.staging || state.width != desc.Width || state.height != desc.Height) {
            state.staging.reset();
            D3D11_TEXTURE2D_DESC stagingDesc = desc;
            stagingDesc.Usage = D3D11_USAGE_STAGING;
            stagingDesc.BindFlags = 0;
            stagingDesc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
            stagingDesc.MiscFlags = 0;
            stagingDesc.MipLevels = 1;
            stagingDesc.ArraySize = 1;
            stagingDesc.SampleDesc.Count = 1;
            if (FAILED(device_->CreateTexture2D(&stagingDesc, nullptr, state.staging.put()))) {
                mutableLastError() = "could not create the read-back texture";
                return false;
            }
            state.width = desc.Width;
            state.height = desc.Height;

            if (!described_ && traceCapture()) {
                // Printed once, because the relationship between the desktop
                // rectangle, the duplication image and the panel orientation is
                // the thing that has to be right for a rotated display, and it is
                // invisible from the outside.
                described_ = true;
                fprintf(stderr,
                        "[capture] dxgi: desktop=%dx%d rotation=%u image=%ux%u "
                        "mode=%ux%u format=%#x misc=%#x bind=%#x samples=%u\n",
                        rectWidth(bounds_), rectHeight(bounds_), rotation_, desc.Width,
                        desc.Height, state.desc.ModeDesc.Width, state.desc.ModeDesc.Height,
                        desc.Format, desc.MiscFlags, desc.BindFlags, desc.SampleDesc.Count);
            }
        }

        context_->CopyResource(state.staging.get(), texture.get());
        // The copy is queued rather than carried out, so the context is flushed
        // before the map. Without this the staging texture is read before the copy
        // has run and every frame comes back black.
        context_->Flush();

        D3D11_MAPPED_SUBRESOURCE mapped{};
        if (FAILED(context_->Map(state.staging.get(), 0, D3D11_MAP_READ, 0, &mapped))) {
            mutableLastError() = "could not map the read-back texture";
            return false;
        }
        blit(state, static_cast<const uint8_t*>(mapped.pData), mapped.RowPitch, desc.Width,
             desc.Height);
        context_->Unmap(state.staging.get(), 0);

        if (!describedPixels_ && traceCapture()) {
            // The read-back is the step that silently produces black frames, so
            // the first frame reports what came out of the map.
            describedPixels_ = true;
            const uint8_t* first = static_cast<const uint8_t*>(mapped.pData);
            fprintf(stderr,
                    "[capture] dxgi: mapped rowPitch=%u first=%02X%02X%02X%02X "
                    "second=%02X%02X%02X%02X\n",
                    mapped.RowPitch, first[0], first[1], first[2], first[3], first[4],
                    first[5], first[6], first[7]);
        }

        presentTicks_ = static_cast<uint64_t>(frameInfo.LastPresentTime.QuadPart);
        accumulatedFrames_ = frameInfo.AccumulatedFrames;
        return true;
    }

    // notePointer remembers where the pointer is. Duplication reports it
    // directly, which is far better than guessing from the regions the
    // compositor moved; the position is only meaningful when the frame also
    // carries a mouse update timestamp.
    void notePointer(const DXGI_OUTDUPL_FRAME_INFO& frameInfo) {
        if (frameInfo.LastMouseUpdateTime.QuadPart == 0) {
            return;
        }
        pointerLeft_ = frameInfo.PointerPosition.Position.x;
        pointerTop_ = frameInfo.PointerPosition.Position.y;
        pointerVisible_ = frameInfo.PointerPosition.Visible != 0;
        pointerKnown_ = true;
    }

    // blit copies one output's rows into the session buffer.
    //
    // Desktop duplication hands the image over already upright: for a rotated
    // display the acquired texture still measures the desktop rectangle, because
    // the driver applies the panel rotation when it presents. The dimensions are
    // therefore compared against the captured rectangle rather than assumed, and
    // only the part that falls inside the region is written.
    void blit(const DuplicationOutput& state, const uint8_t* rows, UINT pitch, UINT width,
              UINT height) {
        const int originX = -bounds_.left;
        const int originY = -bounds_.top;
        const int srcWidth = static_cast<int>(width);
        const int srcHeight = static_cast<int>(height);
        const int outWidth = rectWidth(bounds_);
        const int outHeight = rectHeight(bounds_);
        const size_t rowBytes = static_cast<size_t>(pitch) * height;

        for (int y = 0; y < srcHeight; ++y) {
            const int ty = originY + y;
            if (ty < 0 || ty >= outHeight) {
                continue;
            }
            const uint8_t* src = rows + static_cast<size_t>(y) * pitch;
            uint8_t* dst = pixels_.data() + static_cast<size_t>(ty) * stride_;
            for (int x = 0; x < srcWidth; ++x) {
                const int tx = originX + x;
                if (tx < 0 || tx >= outWidth) {
                    continue;
                }
                const size_t from = static_cast<size_t>(x) * 4;
                if (from + 4 > rowBytes) {
                    break;
                }
                memcpy(dst + static_cast<size_t>(tx) * 4, src + from, 4);
            }
        }
        (void)state;
    }

    // detectCursor turns the pointer position the driver reported into frame
    // coordinates. When the driver reports nothing, the smallest region the
    // compositor moved is the next best guess, because the pointer is the
    // smallest thing on screen that moves.
    void detectCursor() {
        const bool suppressed = hideCursorOnClick_ && primaryMouseDown();
        if (suppressed) {
            cursor_ = RecorderCursor{};
            return;
        }

        if (pointerKnown_ && pointerVisible_) {
            const int left = pointerLeft_ - bounds_.left;
            const int top = pointerTop_ - bounds_.top;
            cursor_.valid = 1;
            cursor_.left = left;
            cursor_.top = top;
            cursor_.right = left + pointerWidth_;
            cursor_.bottom = top + pointerHeight_;
            return;
        }

        if (outputs_.empty()) {
            return;
        }
        DXGI_OUTDUPL_MOVE_RECT rects[16]{};
        UINT count = 0;
        if (FAILED(
                outputs_.front().duplication->GetFrameMoveRects(ARRAYSIZE(rects), rects,
                                                                &count)) ||
            count == 0) {
            return;
        }
        int best = -1;
        long long bestArea = 0;
        for (UINT i = 0; i < count; ++i) {
            const int w = rects[i].DestinationRect.right - rects[i].DestinationRect.left;
            const int h = rects[i].DestinationRect.bottom - rects[i].DestinationRect.top;
            if (w <= 0 || h <= 0) {
                continue;
            }
            const long long area = static_cast<long long>(w) * h;
            if (best < 0 || area < bestArea) {
                best = static_cast<int>(i);
                bestArea = area;
            }
        }
        if (best < 0) {
            return;
        }
        const int left = rects[best].DestinationRect.left - bounds_.left;
        const int top = rects[best].DestinationRect.top - bounds_.top;
        cursor_.valid = 1;
        cursor_.left = left;
        cursor_.top = top;
        cursor_.right = left + (rects[best].DestinationRect.right - rects[best].DestinationRect.left);
        cursor_.bottom = top + (rects[best].DestinationRect.bottom - rects[best].DestinationRect.top);
    }

    static bool primaryMouseDown() {
        return (GetAsyncKeyState(VK_LBUTTON) & 0x8000) != 0;
    }

    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    bool showClick_ = false;
    bool halo_ = false;
    std::vector<DuplicationOutput> outputs_;
    std::vector<uint8_t> pixels_;
    RecorderRect bounds_{0, 0, 0, 0};
    RecorderCursor cursor_{};
    uint32_t rotation_ = 0;
    int stride_ = 0;
    uint64_t presentTicks_ = 0;
    uint32_t accumulatedFrames_ = 0;
    int pointerLeft_ = 0;
    int pointerTop_ = 0;
    int pointerWidth_ = 32;  // reported pointer geometry is not in the frame info
    int pointerHeight_ = 32;
    bool pointerVisible_ = false;
    bool pointerKnown_ = false;
    bool trackCursor_ = false;
    bool hideCursorOnClick_ = false;
    bool described_ = false;
    bool describedPixels_ = false;
};

// GdiCapture is the fallback. GDI never reports new frames, so a grab always
// returns the current screen, and the pointer has to be drawn by hand because
// the hardware pointer is not part of the pixels.
class GdiCapture final : public CaptureSession {
public:
    ~GdiCapture() override { release(); }

    bool open(const RecorderRect& bounds, bool drawCursor, bool hideCursorOnClick,
              uint32_t flags) {
        showClick_ = (flags & RecorderCaptureFlagClickRing) != 0;
        halo_ = (flags & RecorderCaptureFlagHalo) != 0;

        bounds_ = bounds;
        width_ = rectWidth(bounds);
        height_ = rectHeight(bounds);
        if (width_ <= 0 || height_ <= 0) {
            mutableLastError() = "the captured region has no area";
            return false;
        }
        hideCursorOnClick_ = hideCursorOnClick;
        drawCursor_ = drawCursor;

        screenDC_ = GetDC(nullptr);
        if (screenDC_ == nullptr) {
            mutableLastError() = "could not obtain a screen device context";
            return false;
        }
        memoryDC_ = CreateCompatibleDC(screenDC_);
        if (memoryDC_ == nullptr) {
            mutableLastError() = "could not create a memory device context";
            return false;
        }

        BITMAPINFO info{};
        info.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
        info.bmiHeader.biWidth = width_;
        info.bmiHeader.biHeight = -height_;  // negative selects top-down
        info.bmiHeader.biPlanes = 1;
        info.bmiHeader.biBitCount = 32;
        info.bmiHeader.biCompression = BI_RGB;

        void* bits = nullptr;
        // CreateDIBSection returns two distinct things: the HBITMAP the bitmap
        // is selected and deleted through, and the address of the pixels.
        bitmap_ = CreateDIBSection(screenDC_, &info, DIB_RGB_COLORS, &bits, nullptr, 0);
        if (bitmap_ == nullptr || bits == nullptr) {
            mutableLastError() = "could not allocate a 32-bit capture bitmap";
            return false;
        }
        dibBits_ = static_cast<uint8_t*>(bits);
        previous_ = SelectObject(memoryDC_, bitmap_);
        if (previous_ == nullptr) {
            mutableLastError() = "could not select the capture bitmap";
            return false;
        }
        stride_ = alignUp(width_ * 4, 4);
        pixels_.assign(static_cast<size_t>(stride_) * height_, 0);
        return true;
    }

    int32_t grab(int32_t /*timeoutMs*/) override {
        // CAPTUREBLT includes layered and composited windows that plain SRCCOPY
        // would miss, which matters for applications that use them.
        if (!BitBlt(memoryDC_, 0, 0, width_, height_, screenDC_, bounds_.left, bounds_.top,
                    SRCCOPY | CAPTUREBLT)) {
            return fail(RecorderStatusError, "BitBlt failed (error %lu)", GetLastError());
        }
        pointerDrawn_ = false;
        if (drawCursor_ && !(hideCursorOnClick_ && primaryMouseDown())) {
            drawPointer();
        } else {
            cursor_ = RecorderCursor{};
        }
        // GDI draws into the bitmap it allocated, so the frame the caller reads
        // is copied out of it. One copy per frame is the price of the fallback.
        memcpy(pixels_.data(), dibBits_, pixels_.size());
        if (pointerDrawn_ && (halo_ || (showClick_ && primaryMouseDown()))) {
            paintOverlays(pixels_.data(), stride_, width_, height_, pointerX_, pointerY_,
                          halo_, showClick_ && primaryMouseDown());
        }
        return kOk;
    }

    void frame(RecorderFrame* out) const override {
        memset(out, 0, sizeof(*out));
        out->struct_size = sizeof(*out);
        out->width = static_cast<uint32_t>(width_);
        out->height = static_cast<uint32_t>(height_);
        out->stride = stride_;
        out->cursor = cursor_;
    }

    void cursor(RecorderCursor* out) const override {
        *out = cursor_;
    }

    uint32_t backend() const override { return RecorderBackendGdi; }

    const uint8_t* pixels() const override { return pixels_.data(); }

private:
    void release() {
        if (previous_ != nullptr && memoryDC_ != nullptr) {
            SelectObject(memoryDC_, previous_);
            previous_ = nullptr;
        }
        if (bitmap_ != nullptr) {
            DeleteObject(bitmap_);
            bitmap_ = nullptr;
        }
        if (memoryDC_ != nullptr) {
            DeleteDC(memoryDC_);
            memoryDC_ = nullptr;
        }
        if (screenDC_ != nullptr) {
            ReleaseDC(nullptr, screenDC_);
            screenDC_ = nullptr;
        }
    }

    static bool primaryMouseDown() {
        return (GetAsyncKeyState(VK_LBUTTON) & 0x8000) != 0;
    }

    bool pointerGeometry(HCURSOR cursor, int* width, int* height) const {
        if (cursorWidth_ > 0) {
            *width = cursorWidth_;
            *height = cursorHeight_;
            return true;
        }
        ICONINFO info{};
        if (GetIconInfo(cursor, &info) == 0) {
            return false;
        }
        // GetIconInfo hands back copies the caller owns.
        const HBITMAP mask = info.hbmMask;
        BITMAP bitmap{};
        bool ok = GetObject(mask, sizeof(bitmap), &bitmap) != 0 && bitmap.bmWidth > 0 &&
                  bitmap.bmHeight > 0;
        if (mask != nullptr) {
            DeleteObject(mask);
        }
        if (info.hbmColor != nullptr) {
            DeleteObject(info.hbmColor);
        }
        if (!ok) {
            return false;
        }
        cursorWidth_ = bitmap.bmWidth;
        cursorHeight_ = bitmap.bmHeight;
        *width = cursorWidth_;
        *height = cursorHeight_;
        return true;
    }

    void drawPointer() {
        CURSORINFO info{};
        info.cbSize = sizeof(info);
        if (GetCursorInfo(&info) == 0 || (info.flags & CURSOR_SHOWING) == 0) {
            cursor_ = RecorderCursor{};
            return;
        }
        int w = 0;
        int h = 0;
        if (!pointerGeometry(info.hCursor, &w, &h)) {
            return;
        }
        const int x = info.ptScreenPos.x - bounds_.left;
        const int y = info.ptScreenPos.y - bounds_.top;
        DrawIconEx(memoryDC_, x, y, info.hCursor, w, h, 0, nullptr, DI_NORMAL);
        pointerX_ = x;
        pointerY_ = y;
        pointerDrawn_ = true;
        cursor_ = RecorderCursor{1, x, y, x + w, y + h};
    }
    bool showClick_ = false;
    bool halo_ = false;
    bool pointerDrawn_ = false;
    int pointerX_ = 0;
    int pointerY_ = 0;
    bool drawCursor_ = false;
    bool hideCursorOnClick_ = false;
    HDC screenDC_ = nullptr;
    HDC memoryDC_ = nullptr;
    HBITMAP bitmap_ = nullptr;
    HGDIOBJ previous_ = nullptr;
    uint8_t* dibBits_ = nullptr;
    std::vector<uint8_t> pixels_;
    RecorderRect bounds_{0, 0, 0, 0};
    RecorderCursor cursor_{};
    int width_ = 0;
    int height_ = 0;
    int stride_ = 0;
    mutable int cursorWidth_ = 0;
    mutable int cursorHeight_ = 0;
};

// A capture session exposes its pixels through the frame description, and Go
// reads them straight out of this buffer.
}  // namespace

int32_t captureOpen(const RecorderCaptureConfig* config, CaptureSession** out) {
    if (config == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "capture_open: null argument");
    }
    if (config->struct_size != 0 && config->struct_size < sizeof(RecorderCaptureConfig)) {
        return fail(RecorderStatusError, "capture_open: config struct too small (%u bytes)",
                    config->struct_size);
    }
    *out = nullptr;

    ensureCom();
    ComPtr<IDXGIFactory1> factory;
    if (SUCCEEDED(CreateDXGIFactory1(__uuidof(IDXGIFactory1),
                                     reinterpret_cast<void**>(factory.put())))) {
        ComPtr<IDXGIAdapter1> adapter;
        if (SUCCEEDED(factory->EnumAdapters1(config->adapter_index, adapter.put()))) {
            ComPtr<IDXGIOutput> output;
            if (SUCCEEDED(adapter->EnumOutputs(config->output_index, output.put()))) {
                DXGI_OUTPUT_DESC desc{};
                if (SUCCEEDED(output->GetDesc(&desc))) {
                    const RecorderRect bounds{desc.DesktopCoordinates.left,
                                              desc.DesktopCoordinates.top,
                                              desc.DesktopCoordinates.right,
                                              desc.DesktopCoordinates.bottom};
                    if (!config->prefer_gdi) {
                        auto* dxgi = new (std::nothrow) DxgiCapture();
                    if (dxgi != nullptr &&
                        dxgi->open(config->adapter_index, config->output_index,
                                   config->region, config->has_region != 0,
                                   config->track_cursor != 0,
                                   config->hide_cursor_on_click != 0, config->flags)) {
                            *out = dxgi;
                            return kOk;
                        }
                        delete dxgi;
                    }
                    auto* gdi = new (std::nothrow) GdiCapture();
                    if (gdi != nullptr && gdi->open(bounds, config->draw_cursor != 0,
                                                   config->hide_cursor_on_click != 0,
                                                   config->flags)) {
                        *out = gdi;
                        return kOk;
                    }
                    delete gdi;
                }
            }
        }
    }

    const std::string& why = lastError();
    if (why.empty()) {
        return fail(RecorderStatusError,
                    "no capture backend could start for adapter %u output %u",
                    config->adapter_index, config->output_index);
    }
    return fail(RecorderStatusError, "%s", why.c_str());
}

int32_t captureGrab(CaptureSession* capture, int32_t timeoutMs, RecorderFrame* out) {
    if (capture == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "capture_grab: null argument");
    }
    const int32_t status = capture->grab(timeoutMs);
    if (status == kOk) {
        capture->frame(out);
    }
    return status;
}

int32_t captureCursor(CaptureSession* capture, RecorderCursor* out) {
    if (capture == nullptr || out == nullptr) {
        return fail(RecorderStatusError, "capture_cursor: null argument");
    }
    capture->cursor(out);
    return kOk;
}

// A capture session exposes its pixels through the frame description, and Go
// reads them straight out of this buffer.
const uint8_t* capturePixels(const CaptureSession* capture) {
    return capture == nullptr ? nullptr : capture->pixels();
}

}  // namespace recorder
