package audiocodec

import (
	"math"
	"testing"
)

func dbFS(amp float64) float64 {
	return 20 * math.Log10(amp)
}

func TestMeasureOutOfPhaseStereoIsNotSilence(t *testing.T) {
	// Left and right carry identical energy with opposite polarity. A mono
	// mid fold (L+R)/2 cancels to silence; the correct multi-channel
	// measurement must report the real signal level.
	frames := 1024
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		samples[frame*2] = 0.5
		samples[frame*2+1] = -0.5
	}
	clip := &Clip{SampleRate: 48000, Channels: 2, Samples: samples}

	measured := MeasureClip(clip)
	want := dbFS(0.5)
	if math.Abs(measured.PeakDBFS-want) > 0.01 {
		t.Fatalf("anti-phase stereo peak = %.3f dBFS, want %.3f (not the -120 silence floor)", measured.PeakDBFS, want)
	}
	if measured.PeakDBFS <= -100 {
		t.Fatalf("anti-phase stereo was registered as silence: peak %.3f dBFS", measured.PeakDBFS)
	}
	if math.Abs(measured.RMSDBFS-want) > 0.01 {
		t.Fatalf("anti-phase stereo rms = %.3f dBFS, want %.3f", measured.RMSDBFS, want)
	}
}

func TestMeasureKeepsUnbalancedChannelLevels(t *testing.T) {
	// A noticeably weaker right channel must not be hidden by averaging.
	frames := 1024
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		samples[frame*2] = 0.4
		samples[frame*2+1] = 0.1
	}
	measured := Measure(samples)
	wantPeak := dbFS(0.4)
	if math.Abs(measured.PeakDBFS-wantPeak) > 0.01 {
		t.Fatalf("stereo peak = %.3f dBFS, want %.3f", measured.PeakDBFS, wantPeak)
	}
	wantRMS := dbFS(math.Sqrt((0.4*0.4 + 0.1*0.1) / 2))
	if math.Abs(measured.RMSDBFS-wantRMS) > 0.01 {
		t.Fatalf("stereo rms = %.3f dBFS, want %.3f", measured.RMSDBFS, wantRMS)
	}
}

func TestMeasureMonoUnchanged(t *testing.T) {
	samples := make([]float32, 512)
	for index := range samples {
		samples[index] = 0.25
	}
	measured := Measure(samples)
	want := dbFS(0.25)
	if math.Abs(measured.PeakDBFS-want) > 0.01 || math.Abs(measured.RMSDBFS-want) > 0.01 {
		t.Fatalf("mono measurement = %+v, want peak/rms %.3f", measured, want)
	}
}

func TestMeasureEmptyAndSilence(t *testing.T) {
	if got := Measure(nil); got.PeakDBFS != silenceFloorDBFS || got.RMSDBFS != silenceFloorDBFS {
		t.Fatalf("empty measurement = %+v, want silence floor", got)
	}
	clip := &Clip{SampleRate: 48000, Channels: 2, Samples: make([]float32, 8)}
	if got := MeasureClip(clip); got.PeakDBFS != silenceFloorDBFS {
		t.Fatalf("digital silence peak = %.3f, want floor %.3f", got.PeakDBFS, silenceFloorDBFS)
	}
}
