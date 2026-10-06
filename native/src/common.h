// Shared helpers for the native layer: COM lifetime management, thread-local
// error reporting and the string conversions the C ABI needs.
//
// WIN32_LEAN_AND_MEAN and NOMINMAX come from the build script so that every
// translation unit sees the same set; repeating them here only produces
// redefinition warnings.
#pragma once

#include <windows.h>

// WIN32_LEAN_AND_MEAN keeps windows.h from dragging in the whole OLE surface, so
// the pieces the COM layer actually needs are named explicitly.
#include <objbase.h>
#include <unknwn.h>

#include <string>
#include <vector>

#include "recorder_native.h"

namespace recorder {

// Status codes shared with Go through the header.
constexpr int32_t kOk = RecorderStatusOk;

// setError records a message and returns the status that goes with it, so
// callers can `return fail(...)` without repeating themselves.
//
// The message is process-wide rather than per-thread on purpose: Go is free
// to migrate a goroutine between the failing call and the last_error query,
// and a thread-local message would then read as empty on the new thread. Two
// simultaneous failures can swap messages, but a status code never lies about
// which call failed, and the message is diagnostics either way.
int32_t fail(int32_t status, const char* format, ...);

// lastError returns the last recorded message, or an empty string.
std::string& mutableLastError();
std::string lastError();

// toUtf8 converts a wide string, replacing anything unconvertible so the caller
// always receives valid UTF-8 rather than a truncated sequence.
std::string toUtf8(const wchar_t* text);

// copyString writes a NUL terminated UTF-8 string into a fixed size buffer,
// truncating on a character boundary and always leaving room for the terminator.
void copyString(char* destination, uint32_t capacity, const std::string& text);

// Rect helpers shared by the capture backends.
int rectWidth(const RecorderRect& r);
int rectHeight(const RecorderRect& r);
bool rectEmpty(const RecorderRect& r);
RecorderRect rectIntersect(const RecorderRect& a, const RecorderRect& b);
bool rectEqual(const RecorderRect& a, const RecorderRect& b);

// align rounds a byte count up to a multiple of the given alignment, which is
// what GDI and D3D both require of row pitches.
int alignUp(int value, int alignment);

// A COM interface pointer that releases itself. Every COM object the native layer
// holds is wrapped in one of these so an early return cannot leak it.
template <typename T>
class ComPtr {
public:
    ComPtr() = default;
    ~ComPtr() { reset(); }

    ComPtr(const ComPtr&) = delete;
    ComPtr& operator=(const ComPtr&) = delete;

    ComPtr(ComPtr&& other) noexcept : ptr_(other.ptr_) { other.ptr_ = nullptr; }
    ComPtr& operator=(ComPtr&& other) noexcept {
        if (this != &other) {
            reset();
            ptr_ = other.ptr_;
            other.ptr_ = nullptr;
        }
        return *this;
    }

    void reset() {
        if (ptr_ != nullptr) {
            ptr_->Release();
            ptr_ = nullptr;
        }
    }

    T** put() {
        reset();
        return &ptr_;
    }

    T* get() const { return ptr_; }
    T* operator->() const { return ptr_; }
    explicit operator bool() const { return ptr_ != nullptr; }
    // operator! exists so a null check reads as a question rather than a
    // comparison against nullptr, which an explicit conversion cannot answer.
    bool operator!() const { return ptr_ == nullptr; }

    T* detach() {
        T* p = ptr_;
        ptr_ = nullptr;
        return p;
    }

    void attach(T* p) {
        reset();
        ptr_ = p;
    }

private:
    T* ptr_ = nullptr;
};

// queryInterface resolves an interface through QueryInterface, which is how a
// COM object that arrived as a base interface is narrowed to a richer one. The
// SDK's IID_PPV_ARGS needs an lvalue, so the cast is done by hand here.
template <typename Out>
bool queryInterface(IUnknown* source, ComPtr<Out>* out) {
    if (source == nullptr || out == nullptr) {
        return false;
    }
    Out* raw = nullptr;
    const HRESULT hr = source->QueryInterface(__uuidof(Out), reinterpret_cast<void**>(&raw));
    if (FAILED(hr) || raw == nullptr) {
        return false;
    }
    out->attach(raw);
    return true;
}

// comScope initialises COM for the calling thread and undoes it afterwards.
// Only use it when every COM object dies before the scope does.
class ComScope {
  public:
    ComScope();
    ~ComScope();
    ComScope(const ComScope&) = delete;
    ComScope& operator=(const ComScope&) = delete;

  private:
    bool initialised_ = false;
};

// ensureCom initialises COM on the calling thread and deliberately never
// undoes it. Sessions (capture devices, the WIC factory, audio clients) are
// created under one call and driven from later calls, potentially on another
// OS thread after Go migrates the goroutine — undoing COM at the end of open
// would disconnect those objects and crash the next use. One initialisation
// per thread is leaked instead; the process teardown reclaims it.
inline void ensureCom() {
    thread_local const bool initialised = [] {
        const HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        // S_FALSE means COM was already initialised here and RPC_E_CHANGED_MODE
        // means it was initialised single-threaded; either way there is
        // nothing more to do, because the caller's objects are used in place
        // rather than marshalled.
        (void)hr;
        return true;
    }();
    (void)initialised;
}

}  // namespace recorder
