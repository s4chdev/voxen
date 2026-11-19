package audio

import (
	"context"
	"errors"
	"testing"
)

// phantomEngine enumerates devices but cannot open any, which is exactly what a
// CI runner reports: an ALSA "default" with no card, or a WASAPI endpoint with no
// audio service.
type phantomEngine struct{ Null }

func (phantomEngine) Devices(context.Context) ([]Device, error) {
	return []Device{
		{ID: "phantom-in", Name: "Phantom Input", IsInput: true, IsDefault: true},
		{ID: "phantom-out", Name: "Phantom Output", IsOutput: true, IsDefault: true},
	}, nil
}

func (phantomEngine) Version() string { return "miniaudio (phantom)" }

func (phantomEngine) OpenCapture(context.Context, DeviceID) (CaptureStream, error) {
	return nil, errors.New("Failed to open backend device")
}

func (phantomEngine) OpenPlayback(context.Context, DeviceID) (PlaybackStream, error) {
	return nil, errors.New("Failed to open backend device")
}

// TestProbeRejectsPhantomDevices is the regression test for the CI failure that
// made audio look like a product defect when the machine simply had no hardware.
func TestProbeRejectsPhantomDevices(t *testing.T) {
	probe := Probe(context.Background(), phantomEngine{})
	if probe.Devices != 2 {
		t.Fatalf("probe should have enumerated the phantom devices, got %d", probe.Devices)
	}
	if probe.Usable() {
		t.Fatal("phantom devices were reported usable; the capability matrix would then lie")
	}
	if probe.CaptureOK || probe.PlaybackOK {
		t.Fatalf("a direction reported success without opening: %+v", probe)
	}
	if probe.Err == nil {
		t.Fatal("probe should explain why audio is not usable")
	}
	if probe.Backend != "miniaudio (phantom)" {
		t.Fatalf("backend = %q", probe.Backend)
	}
}

// TestProbeNullEngine covers a build without cgo.
func TestProbeNullEngine(t *testing.T) {
	probe := Probe(context.Background(), Null{})
	if probe.Usable() {
		t.Fatal("the null engine must never be reported usable")
	}
	if probe.Backend != "unavailable" {
		t.Fatalf("backend = %q, want unavailable", probe.Backend)
	}
}

// TestProbePermissionIsDistinguished proves a denied microphone is reported as a
// permission problem, because that is the one case the user can fix.
func TestProbePermissionIsDistinguished(t *testing.T) {
	probe := Probe(context.Background(), permissionEngine{})
	if !probe.Permission {
		t.Fatal("a denied microphone must be reported as a permission problem")
	}
	if probe.Usable() {
		t.Fatal("a denied microphone must not be reported usable")
	}
}

type permissionEngine struct{ Null }

func (permissionEngine) Version() string { return "miniaudio (denied)" }

func (permissionEngine) OpenCapture(context.Context, DeviceID) (CaptureStream, error) {
	return nil, ErrPermission
}
