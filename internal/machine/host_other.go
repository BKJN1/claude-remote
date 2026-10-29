//go:build !windows

package machine

// Host exists so the code builds (go vet in CI) off Windows; it can do nothing.
type Host struct{}

func New() *Host { return &Host{} }

func (*Host) Status() (Status, error)        { return Status{}, ErrUnsupported }
func (*Host) Lock() error                    { return ErrUnsupported }
func (*Host) Sleep(bool) error               { return ErrUnsupported }
func (*Host) Shutdown(bool, int, bool) error { return ErrUnsupported }
func (*Host) CancelShutdown() error          { return ErrUnsupported }
func (*Host) SetKeepAwake(bool) error        { return ErrUnsupported }
func (*Host) KeepAwake() bool                { return false }
