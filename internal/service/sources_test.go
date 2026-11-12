package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/storage"
)

func newTestService(t *testing.T) (*Service, *storage.Repository) {
	t.Helper()
	repo, err := storage.NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	return New(repo, DefaultConfig()), repo
}

func constantStereoWAV(t *testing.T, frames int, left, right float32) []byte {
	t.Helper()
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		samples[frame*2] = left
		samples[frame*2+1] = right
	}
	wav, err := audiocodec.EncodePCM16(48000, 2, samples)
	if err != nil {
		t.Fatalf("encode wav: %v", err)
	}
	return wav
}

func registerWAV(t *testing.T, svc *Service, id, name string, wav []byte) domain.SourceAsset {
	t.Helper()
	asset, created, err := svc.CreateSource(context.Background(), id, SourceWriteRequest{
		Name:      name,
		WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		t.Fatalf("create source %s: %v", id, err)
	}
	if !created {
		t.Fatalf("source %s was not created", id)
	}
	return asset
}

func writeLegacySourceMetadata(t *testing.T, repo *storage.Repository, id string, wav []byte, peak, rms float64, frames int64) {
	t.Helper()
	dir := filepath.Join(repo.Root(), "sources", id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.wav"), wav, 0o640); err != nil {
		t.Fatalf("write media: %v", err)
	}
	digest := sha256.Sum256(wav)
	asset := domain.SourceAsset{
		ID: id, Name: "legacy " + id, Digest: hex.EncodeToString(digest[:]),
		ByteCount: int64(len(wav)), SampleRate: 48000, Channels: 2,
		BitsPerSample: 16, FrameCount: frames, DurationMS: frames * 1000 / 48000,
		PeakDBFS: peak, RMSDBFS: rms,
	}
	payload, err := json.Marshal(asset)
	if err != nil {
		t.Fatalf("marshal legacy metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), payload, 0o640); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

func TestCreateSourceMeasuresAntiPhaseStereoCorrectly(t *testing.T) {
	svc, _ := newTestService(t)
	wav := constantStereoWAV(t, 480, 0.5, -0.5)
	asset := registerWAV(t, svc, "antiphase_01", "anti phase take", wav)

	want := 20 * math.Log10(0.5)
	if math.Abs(asset.PeakDBFS-want) > 0.02 {
		t.Fatalf("registered anti-phase peak = %.3f dBFS, want %.3f (not silence)", asset.PeakDBFS, want)
	}
	if math.Abs(asset.RMSDBFS-want) > 0.02 {
		t.Fatalf("registered anti-phase rms = %.3f dBFS, want %.3f", asset.RMSDBFS, want)
	}
	if asset.MeasurementsVersion != domain.MeasurementsVersionV2 {
		t.Fatalf("measurements version = %d, want %d", asset.MeasurementsVersion, domain.MeasurementsVersionV2)
	}
}

func TestReUploadingSameRecordingReturnsCorrectedReadings(t *testing.T) {
	svc, repo := newTestService(t)
	wav := constantStereoWAV(t, 480, 0.5, -0.5)
	writeLegacySourceMetadata(t, repo, "stale_01", wav, -120, -120, 480)

	// Re-uploading the exact same bytes must be idempotent yet surface the
	// corrected measurements instead of the stored near-silence readings.
	asset, created, err := svc.CreateSource(context.Background(), "stale_01", SourceWriteRequest{
		Name:      "legacy stale_01",
		WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		t.Fatalf("idempotent re-upload: %v", err)
	}
	if created {
		t.Fatal("identical re-upload must not create a duplicate source")
	}
	want := 20 * math.Log10(0.5)
	if math.Abs(asset.PeakDBFS-want) > 0.02 {
		t.Fatalf("re-upload peak = %.3f dBFS, want %.3f", asset.PeakDBFS, want)
	}
	if asset.MeasurementsVersion != domain.MeasurementsVersionV2 {
		t.Fatalf("re-upload did not heal measurements version %d", asset.MeasurementsVersion)
	}
}

func TestRendersAreIdenticalBeforeAndAfterBackfill(t *testing.T) {
	svc, repo := newTestService(t)
	// Two legacy sources plus a correctly registered one share a timeline.
	// Rendering lazily heals only the sources it references, so after the first
	// render some records stay un-migrated and the startup backfill still has
	// real work to do.
	legacyFirst := constantStereoWAV(t, 2400, 0.4, 0.1)
	writeLegacySourceMetadata(t, repo, "legacy_first", legacyFirst, 20*math.Log10(0.25), 20*math.Log10(0.25), 2400)
	legacySecond := constantStereoWAV(t, 2400, 0.3, -0.3)
	writeLegacySourceMetadata(t, repo, "legacy_second", legacySecond, -120, -120, 2400)
	freshWAV := constantStereoWAV(t, 2400, 0.2, 0.2)
	registerWAV(t, svc, "fresh_span", "fresh take", freshWAV)

	plan := domain.Timeline{
		ID: "mixed_timeline", Name: "mixed era timeline", SampleRate: 48000,
		OutputChannels: 2, TargetPeakDBFS: -1, State: "ready",
		Spans: []domain.TimelineSpan{
			{SourceID: "legacy_first", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0},
			{SourceID: "legacy_second", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0},
			{SourceID: "fresh_span", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0},
		},
	}
	if err := repo.SaveTimeline(plan); err != nil {
		t.Fatalf("save timeline: %v", err)
	}

	renderOnce := func(id string) []byte {
		job, created, err := svc.CreateRender(context.Background(), id, RenderWriteRequest{TimelineID: "mixed_timeline"})
		if err != nil || !created || job.State != domain.RenderCompleted {
			t.Fatalf("render %s: state=%s err=%v", id, job.State, err)
		}
		output, _, err := repo.GetRenderOutput(id)
		if err != nil {
			t.Fatalf("read output %s: %v", id, err)
		}
		return output
	}

	before := renderOnce("render_before_backfill")
	// The render lazily healed the two referenced legacy sources; the startup
	// backfill must find the remaining dataset already consistent and change
	// nothing, while earlier it still had un-migrated records to converge.
	repaired, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if repaired < 0 || repaired > 0 {
		t.Fatalf("backfill repaired %d, want 0 after render-time healing", repaired)
	}
	after := renderOnce("render_after_backfill")

	// Rendering always decodes the original WAV, so amplitude semantics cannot
	// depend on which measurement era the metadata happens to be in.
	if sha256Hex(before) != sha256Hex(after) {
		t.Fatal("rendered output differs before and after measurement backfill: a work cannot mix old and new amplitude meanings")
	}

	// And every re-render stays fully deterministic.
	again := renderOnce("render_repeated")
	if sha256Hex(after) != sha256Hex(again) {
		t.Fatal("repeated render of the same timeline is not deterministic")
	}
}

func TestBackfillConvergesUnreferencedLegacySources(t *testing.T) {
	svc, repo := newTestService(t)
	// A legacy source that no render references must still be repaired by the
	// startup backfill, without anyone re-registering it.
	orphanWAV := constantStereoWAV(t, 1200, 0.5, -0.5)
	writeLegacySourceMetadata(t, repo, "orphan_legacy", orphanWAV, -120, -120, 1200)
	repaired, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil || repaired != 1 {
		t.Fatalf("backfill = (%d, %v), want 1 orphan repaired", repaired, err)
	}
	asset, err := svc.GetSource(context.Background(), "orphan_legacy")
	if err != nil {
		t.Fatalf("read orphan: %v", err)
	}
	want := 20 * math.Log10(0.5)
	if math.Abs(asset.PeakDBFS-want) > 0.02 || asset.MeasurementsVersion != domain.MeasurementsVersionV2 {
		t.Fatalf("orphan after backfill: peak=%.3f version=%d, want %.3f / v2", asset.PeakDBFS, asset.MeasurementsVersion, want)
	}
}

func TestDeliveredArtifactsRemainImmutable(t *testing.T) {
	svc, repo := newTestService(t)
	wav := constantStereoWAV(t, 1200, 0.35, 0.15)
	registerWAV(t, svc, "delivered_src", "delivered take", wav)
	if _, _, err := svc.CreateTimeline(context.Background(), "delivered_tl", TimelineWriteRequest{
		Name: "delivered", SampleRate: 48000, OutputChannels: 2, TargetPeakDBFS: -2,
		Spans: []domain.TimelineSpan{{SourceID: "delivered_src", SourceStartFrame: 0, SourceEndFrame: 1200}},
	}); err != nil {
		t.Fatalf("create timeline: %v", err)
	}
	if _, created, err := svc.CreateRender(context.Background(), "delivered_job", RenderWriteRequest{TimelineID: "delivered_tl"}); err != nil || !created {
		t.Fatalf("create render: err=%v created=%v", err, created)
	}
	outputBefore, receiptBefore, err := repo.GetRenderOutput("delivered_job")
	if err != nil {
		t.Fatalf("read delivered artifacts: %v", err)
	}

	// Healing/backfill activity must never rewrite a completed render.
	if _, err := repo.BackfillSourceMeasurements(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, err := svc.GetReceipt(context.Background(), "delivered_job"); err != nil {
		t.Fatalf("receipt became unavailable: %v", err)
	}
	outputAfter, receiptAfter, err := repo.GetRenderOutput("delivered_job")
	if err != nil {
		t.Fatalf("reread delivered artifacts: %v", err)
	}
	if sha256Hex(outputBefore) != sha256Hex(outputAfter) {
		t.Fatal("delivered output WAV was modified after publication")
	}
	if receiptBefore.Output.Digest != receiptAfter.Output.Digest ||
		receiptBefore.Normalization.OutputPeakDBFS != receiptAfter.Normalization.OutputPeakDBFS {
		t.Fatal("historical receipt was modified after publication")
	}
}

func sha256Hex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
