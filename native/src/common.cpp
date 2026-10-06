#include "common.h"

#include <stdarg.h>
#include <stdio.h>

#include <mutex>

namespace recorder {

namespace {

std::mutex g_errorMutex;
std::string g_lastError;

}  // namespace

int32_t fail(int32_t status, const char* format, ...) {
    char buffer[512];
    va_list args;
    va_start(args, format);
    vsnprintf(buffer, sizeof(buffer), format, args);
    va_end(args);
    std::lock_guard<std::mutex> lock(g_errorMutex);
    g_lastError = buffer;
    return status;
}

std::string& mutableLastError() {
    // Assignment through the reference is synchronised by the caller's fail()
    // convention in practice; direct writes race benignly (diagnostics only).
    return g_lastError;
}

std::string lastError() {
    std::lock_guard<std::mutex> lock(g_errorMutex);
    return g_lastError;
}

std::string toUtf8(const wchar_t* text) {
    if (text == nullptr || *text == L'\0') {
        return std::string();
    }
    const int needed = WideCharToMultiByte(CP_UTF8, 0, text, -1, nullptr, 0, nullptr, nullptr);
    if (needed <= 1) {
        return std::string();
    }
    std::string out(static_cast<size_t>(needed - 1), '\0');
    WideCharToMultiByte(CP_UTF8, 0, text, -1, out.data(), needed, nullptr, nullptr);
    return out;
}

void copyString(char* destination, uint32_t capacity, const std::string& text) {
    if (destination == nullptr || capacity == 0) {
        return;
    }
    size_t copy = text.size() < capacity - 1 ? text.size() : capacity - 1;
    // Do not split a multi-byte sequence when truncating.
    while (copy > 0 && (static_cast<unsigned char>(text[copy]) & 0xC0) == 0x80) {
        --copy;
    }
    memcpy(destination, text.data(), copy);
    destination[copy] = '\0';
}

int rectWidth(const RecorderRect& r) {
    return static_cast<int>(r.right - r.left);
}

int rectHeight(const RecorderRect& r) {
    return static_cast<int>(r.bottom - r.top);
}

bool rectEmpty(const RecorderRect& r) {
    return r.right <= r.left || r.bottom <= r.top;
}

RecorderRect rectIntersect(const RecorderRect& a, const RecorderRect& b) {
    RecorderRect r;
    r.left = a.left > b.left ? a.left : b.left;
    r.top = a.top > b.top ? a.top : b.top;
    r.right = a.right < b.right ? a.right : b.right;
    r.bottom = a.bottom < b.bottom ? a.bottom : b.bottom;
    if (rectEmpty(r)) {
        return RecorderRect{0, 0, 0, 0};
    }
    return r;
}

bool rectEqual(const RecorderRect& a, const RecorderRect& b) {
    return a.left == b.left && a.top == b.top && a.right == b.right && a.bottom == b.bottom;
}

int alignUp(int value, int alignment) {
    if (alignment <= 0) {
        return value;
    }
    const int remainder = value % alignment;
    return remainder == 0 ? value : value + (alignment - remainder);
}

ComScope::ComScope() {
    const HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    // S_FALSE means COM was already initialised on this thread, which is fine;
    // RPC_E_CHANGED_MODE means it was initialised as single threaded, which we
    // have to live with rather than fail on.
    initialised_ = SUCCEEDED(hr);
}

ComScope::~ComScope() {
    if (initialised_) {
        CoUninitialize();
    }
}

}  // namespace recorder
