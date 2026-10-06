# Builds the program art from 3.png (icons) and 2.png (background).
#
#   icon-app.ico   taskbar button, title-bar icon, exe icon, window icon
#   icon-tray.ico  notification area while idle
#   icon-rec.ico   notification area while recording (red dot)
#   bg.bmp         control-window background, client-sized and pre-dimmed so
#                  labels stay readable with no runtime alpha cost
#
# Usage: powershell -ExecutionPolicy Bypass -File assets\make-icons.ps1
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$outDir = Join-Path $root 'assets'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

function New-SquareBase($path, [double]$fill) {
    # Center-crop to square, then inset to $fill so nothing kisses the edge
    # at 16 px. Transparent padding; the art carries its own dark ground.
    $source = [System.Drawing.Image]::FromFile($path)
    try {
        $side = [Math]::Min($source.Width, $source.Height)
        $sx = [int](($source.Width - $side) / 2)
        $sy = [int](($source.Height - $side) / 2)
        $inner = [int]($side * $fill)
        $pad = [int](($side - $inner) / 2)
        $canvas = New-Object System.Drawing.Bitmap($side, $side,
            [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
        $g = [System.Drawing.Graphics]::FromImage($canvas)
        try {
            $g.Clear([System.Drawing.Color]::Transparent)
            $g.CompositingQuality = [System.Drawing.Drawing2D.CompositingQuality]::HighQuality
            $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
            $g.DrawImage($source,
                (New-Object System.Drawing.Rectangle($pad, $pad, $inner, $inner)),
                (New-Object System.Drawing.Rectangle($sx, $sy, $side, $side)),
                [System.Drawing.GraphicsUnit]::Pixel)
        } finally {
            $g.Dispose()
        }
        return $canvas
    } finally {
        $source.Dispose()
    }
}

function Add-RecDot($bmp) {
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    try {
        $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
        $r = [int]($bmp.Width * 0.115)
        $cx = $bmp.Width - [int]($bmp.Width * 0.15)
        $cy = $bmp.Height - [int]($bmp.Height * 0.15)
        $white = New-Object System.Drawing.SolidBrush ([System.Drawing.Color]::White)
        try {
            $g.FillEllipse($white, $cx - $r - 7, $cy - $r - 7, ($r + 7) * 2, ($r + 7) * 2)
        } finally {
            $white.Dispose()
        }
        $red = New-Object System.Drawing.SolidBrush ([System.Drawing.Color]::FromArgb(232, 17, 35))
        try {
            $g.FillEllipse($red, $cx - $r, $cy - $r, $r * 2, $r * 2)
        } finally {
            $red.Dispose()
        }
    } finally {
        $g.Dispose()
    }
}

function Get-PngBytes($bmp, [int]$size) {
    $scaled = New-Object System.Drawing.Bitmap($size, $size,
        [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $g = [System.Drawing.Graphics]::FromImage($scaled)
    try {
        $g.CompositingQuality = [System.Drawing.Drawing2D.CompositingQuality]::HighQuality
        $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
        $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
        $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
        $g.DrawImage($bmp, 0, 0, $size, $size)
    } finally {
        $g.Dispose()
    }
    $ms = New-Object System.IO.MemoryStream
    try {
        $scaled.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
        return $ms.ToArray()
    } finally {
        $ms.Dispose()
        $scaled.Dispose()
    }
}

function Write-Icon($bitmaps, $path) {
    # ICO with PNG-compressed entries reads on everything from Vista on.
    $fs = [System.IO.File]::Create($path)
    try {
        $bw = New-Object System.IO.BinaryWriter $fs
        $bw.Write([uint16]0)
        $bw.Write([uint16]1)
        $bw.Write([uint16]$bitmaps.Count)
        $offset = 6 + 16 * $bitmaps.Count
        foreach ($b in $bitmaps) {
            $s = $b.Size
            if ($s -eq 256) { $s = 0 }
            $bw.Write([byte]$s)
            $bw.Write([byte]$s)
            $bw.Write([byte]0)
            $bw.Write([byte]0)
            $bw.Write([uint16]1)
            $bw.Write([uint16]32)
            $bw.Write([uint32]$b.Png.Length)
            $bw.Write([uint32]$offset)
            $offset += $b.Png.Length
        }
        foreach ($b in $bitmaps) {
            $bw.Write($b.Png, 0, $b.Png.Length)
        }
        $bw.Flush()
    } finally {
        $fs.Close()
    }
}

$sizes = @(16, 24, 32, 48, 64, 128, 256)

$base = New-SquareBase (Join-Path $root '3.png') 0.92
try {
    $app = foreach ($s in $sizes) {
        [pscustomobject]@{ Size = $s; Png = (Get-PngBytes $base $s) }
    }
    Write-Icon $app (Join-Path $outDir 'icon-app.ico')
    Write-Icon $app (Join-Path $outDir 'icon-tray.ico')
} finally {
    $base.Dispose()
}

$rec = New-SquareBase (Join-Path $root '3.png') 0.92
try {
    Add-RecDot $rec
    $recList = foreach ($s in $sizes) {
        [pscustomobject]@{ Size = $s; Png = (Get-PngBytes $rec $s) }
    }
    Write-Icon $recList (Join-Path $outDir 'icon-rec.ico')
} finally {
    $rec.Dispose()
}

# Background: cover-fit the 1024 art into 480x540, then lay 70% black over it
# so labels and controls stay readable. Baked once; the window just blits.
$art = [System.Drawing.Image]::FromFile((Join-Path $root '2.png'))
try {
    $bw, $bh = 480, 540
    $bg = New-Object System.Drawing.Bitmap($bw, $bh,
        [System.Drawing.Imaging.PixelFormat]::Format24bppRgb)
    $g = [System.Drawing.Graphics]::FromImage($bg)
    try {
        $g.CompositingQuality = [System.Drawing.Drawing2D.CompositingQuality]::HighQuality
        $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
        $scale = [Math]::Max($bw / $art.Width, $bh / $art.Height)
        $dw = [int]($art.Width * $scale)
        $dh = [int]($art.Height * $scale)
        $dx = [int](($bw - $dw) / 2)
        $dy = [int](($bh - $dh) / 2)
        $g.DrawImage($art, $dx, $dy, $dw, $dh)
        $dim = New-Object System.Drawing.SolidBrush ([System.Drawing.Color]::FromArgb(178, 8, 10, 16))
        try {
            $g.FillRectangle($dim, 0, 0, $bw, $bh)
        } finally {
            $dim.Dispose()
        }
    } finally {
        $g.Dispose()
    }
    $bg.Save((Join-Path $outDir 'bg.bmp'), [System.Drawing.Imaging.ImageFormat]::Bmp)
    $bg.Dispose()
} finally {
    $art.Dispose()
}

Get-ChildItem $outDir | Select-Object Name, Length
