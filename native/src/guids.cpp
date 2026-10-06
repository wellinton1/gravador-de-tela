// Direct3D, WASAPI, WIC and Media Foundation publish their GUIDs as extern
// declarations, and exactly one translation unit has to define them. That is
// this file: it defines INITGUID before including the headers, and includes
// nothing else.
#define INITGUID

#include <mmdeviceapi.h>
#include <audioclient.h>
#include <d3d11.h>
#include <dxgi1_2.h>
#include <wincodec.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <wmcodecdsp.h>

// The capture client's interface identifier is declared by the SDK as an extern
// that no import library shipped here provides, so the published value is
// defined directly. It is the same number the SDK documents.
const GUID IID_IAudioCaptureClient = {0xc8adbd64,
                                     0xe71e,
                                     0x48a0,
                                     {0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}};
