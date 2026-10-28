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
