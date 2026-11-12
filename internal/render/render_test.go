package render

import (
	"context"
	"math"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

const renderSampleRate = 48000

func stereoWAV(t *testing.T, frames int, fill func(frame int) (float32, float32)) []byte {
	t.Helper()
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		left, right := fill(frame)
		samples[frame*2] = left
		samples[frame*2+1] = right
	}
	wav, err := audiocodec.EncodePCM16(renderSampleRate, 2, samples)
	if err != nil {
		t.Fatalf("encode stereo wav: %v", err)
	}
	return wav
}

func renderPlan(target float64, spans ...domain.TimelineSpan) domain.Timeline {
	return domain.Timeline{
		ID:             "timeline_under_test",
		Name:           "test timeline",
		SampleRate:     renderSampleRate,
		OutputChannels: 2,
		TargetPeakDBFS: target,
		Spans:          spans,
		State:          "ready",
	}
}

// A stereo source whose right channel is 4x quieter than the left must keep
// that relationship in the rendered output; the removed per-channel peak
// matching used to raise the quiet channel until both were equally loud.
func TestRenderPreservesStereoChannelBalance(t *testing.T) {
	frames := 4800
	wav := stereoWAV(t, frames, func(int) (float32, float32) { return 0.4, 0.1 })
	plan := renderPlan(-1.0, domain.TimelineSpan{
		SourceID: "unbalanced", SourceStartFrame: 0, SourceEndFrame: int64(frames),
	})
	result, err := Render(context.Background(), "render_balance", plan, map[string]SourceInput{
		"unbalanced": {Asset: domain.SourceAsset{ID: "unbalanced", Digest: "x", FrameCount: int64(frames), SampleRate: renderSampleRate}, WAV: wav},
	}, Limits{}, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	decoded, _, err := audiocodec.DecodeWAV(result.WAV)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	for frame := 0; frame < 10; frame++ {
		left := float64(decoded.Samples[frame*2])
		right := float64(decoded.Samples[frame*2+1])
		ratio := right / left
		if math.Abs(ratio-0.25) > 1e-3 {
			t.Fatalf("frame %d R/L ratio = %.4f, want 0.25 (original channel relationship)", frame, ratio)
		}
	}
	measured := audiocodec.Measure(decoded.Samples)
	if math.Abs(measured.PeakDBFS-(-1.0)) > 0.25 {
		t.Fatalf("output peak %.3f dBFS does not meet the -1 dBFS whole-work target", measured.PeakDBFS)
	}
	if math.Abs(result.Receipt.Normalization.OutputPeakDBFS-(-1.0)) > 0.25 {
		t.Fatalf("receipt peak %.3f dBFS does not match the target", result.Receipt.Normalization.OutputPeakDBFS)
	}
}

// Crossfade weights are shared by both channels, so the pan must not jump at
// the overlap boundary or anywhere inside the fade. The expected output is the
// equal-power mix of the original channel values; per-channel rebalancing
// would make the right channel jump to a level the source never had.
func TestRenderCrossfadeKeepsPanStable(t *testing.T) {
	frames := 4800
	crossfade := int64(2400)
	// Left carries a transient peak; right stays steady and quiet.
	first := stereoWAV(t, frames, func(frame int) (float32, float32) {
		if frame == frames-1 {
			return 0.4, 0.1
		}
		return 0.1, 0.1
	})
	second := stereoWAV(t, frames, func(int) (float32, float32) { return 0.2, 0.2 })
	plan := renderPlan(-1.0,
		domain.TimelineSpan{SourceID: "a", SourceStartFrame: 0, SourceEndFrame: int64(frames), CrossfadeFrames: 0},
		domain.TimelineSpan{SourceID: "b", SourceStartFrame: 0, SourceEndFrame: int64(frames), CrossfadeFrames: crossfade},
	)
	sources := map[string]SourceInput{
		"a": {Asset: domain.SourceAsset{ID: "a", Digest: "a", FrameCount: int64(frames), SampleRate: renderSampleRate}, WAV: first},
		"b": {Asset: domain.SourceAsset{ID: "b", Digest: "b", FrameCount: int64(frames), SampleRate: renderSampleRate}, WAV: second},
	}
	result, err := Render(context.Background(), "render_pan", plan, sources, Limits{}, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	decoded, _, err := audiocodec.DecodeWAV(result.WAV)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if decoded.FrameCount() != int64(frames*2-int(crossfade)) {
		t.Fatalf("output frames = %d, want %d", decoded.FrameCount(), frames*2-int(crossfade))
	}

	// Derive the normalization gain from the known global pre-normalization
	// peak. The loudest point is inside the equal-power overlap, where the two
	// span values 0.1 and 0.2 combine as sqrt(0.1^2 + 0.2^2) ≈ 0.2236; the
	// target is -1 dBFS.
	preNormPeak := math.Sqrt(0.1*0.1 + 0.2*0.2)
	gain := audiocodec.DBToLinear(-1.0) / preNormPeak
	const tolerance = 6e-4
	overlapStart := int64(frames) - crossfade
	for frame := int64(0); frame < crossfade; frame++ {
		progress := (float64(frame) + 0.5) / float64(crossfade)
		angle := progress * math.Pi / 2
		incoming := math.Sin(angle)
		outgoing := math.Cos(angle)
		wantRight := (0.1*outgoing + 0.2*incoming) * gain
		gotRight := float64(decoded.Samples[(overlapStart+frame)*2+1])
		if math.Abs(gotRight-wantRight) > tolerance {
			t.Fatalf("crossfade frame %d right = %.5f, want equal-power mix %.5f (pan must not jump)", frame, gotRight, wantRight)
		}
	}

	// The step across the overlap boundary must not be larger than typical
	// in-fade steps.
	largestInnerStep := 0.0
	for frame := overlapStart + 1; frame < overlapStart+crossfade; frame++ {
		step := math.Abs(float64(decoded.Samples[frame*2+1]) - float64(decoded.Samples[(frame-1)*2+1]))
		if step > largestInnerStep {
			largestInnerStep = step
		}
	}
	boundaryStep := math.Abs(float64(decoded.Samples[(overlapStart+crossfade)*2+1]) - float64(decoded.Samples[(overlapStart+crossfade-1)*2+1]))
	if boundaryStep > largestInnerStep*2+2e-4 {
		t.Fatalf("pan jump at crossfade boundary: step %.5f vs largest in-fade step %.5f", boundaryStep, largestInnerStep)
	}
}

func TestPrepareSpansDoesNotRebalanceChannels(t *testing.T) {
	frames := 2400
	wav := stereoWAV(t, frames, func(int) (float32, float32) { return 0.4, 0.1 })
	plan := renderPlan(-1.0, domain.TimelineSpan{
		SourceID: "unbalanced", SourceStartFrame: 0, SourceEndFrame: int64(frames),
	})
	spans, total, err := prepareSpans(context.Background(), plan, map[string]SourceInput{
		"unbalanced": {Asset: domain.SourceAsset{ID: "unbalanced", FrameCount: int64(frames), SampleRate: renderSampleRate}, WAV: wav},
	})
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if total != int64(frames) || len(spans) != 1 {
		t.Fatalf("unexpected preparation: frames=%d spans=%d", total, len(spans))
	}
	left := float64(spans[0].Clip.Samples[0])
	right := float64(spans[0].Clip.Samples[1])
	if math.Abs(left-0.4) > 1e-4 || math.Abs(right-0.1) > 1e-4 {
		t.Fatalf("prepared span samples = (%.4f, %.4f), want original (0.4, 0.1)", left, right)
	}
}
