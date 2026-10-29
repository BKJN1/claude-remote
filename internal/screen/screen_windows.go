//go:build windows

package screen

import (
	"context"
	"fmt"

	"github.com/kbinani/screenshot"
)

// Capture grabs display (0 = primary) and returns a PNG. kbinani/screenshot
// wraps GDI BitBlt: much simpler than DXGI duplication and good enough for
// "what is on my screen". It cannot be cancelled mid-call, so the caller runs
// it in a goroutine with a timeout. Locked screens and DRM windows come out black.
func Capture(ctx context.Context, display int, scale float64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := screenshot.NumActiveDisplays()
	if display < 0 || display >= n {
		return nil, fmt.Errorf("display %d out of range (have %d)", display, n)
	}
	img, err := screenshot.CaptureDisplay(display)
	if err != nil {
		return nil, err
	}
	return Encode(img, scale)
}

// Displays lists the active monitors in capture order (0 is the primary).
func Displays() []Display {
	n := screenshot.NumActiveDisplays()
	out := make([]Display, 0, n)
	for i := 0; i < n; i++ {
		b := screenshot.GetDisplayBounds(i)
		out = append(out, Display{Index: i, Width: b.Dx(), Height: b.Dy()})
	}
	return out
}
