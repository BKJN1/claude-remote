//go:build !windows

package screen

import (
	"context"
	"errors"
)

// Capture exists so the code builds (go vet in CI) off Windows.
func Capture(ctx context.Context, display int, scale float64) ([]byte, error) {
	return nil, errors.New("screenshots are only supported on Windows")
}

// Displays reports no monitors off Windows.
func Displays() []Display { return nil }
