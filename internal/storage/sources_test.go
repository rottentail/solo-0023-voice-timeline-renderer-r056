package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	repo, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	return repo
}

func stereoWAVBytes(t *testing.T, frames int, left, right float32) []byte {
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

// writeLegacySource stores a source as the pre-fix build would have: media
// plus metadata without a measurements_version marker and with mid-downmix
// numbers.
func writeLegacySource(t *testing.T, repo *Repository, id string, wav []byte, legacyPeak, legacyRMS float64, channels int, frames int64) domain.SourceAsset {
	t.Helper()
	dir := repo.path("sources", id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir legacy source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.wav"), wav, 0o640); err != nil {
		t.Fatalf("write legacy media: %v", err)
	}
	asset := domain.SourceAsset{
		ID:            id,
		Name:          "legacy " + id,
		Digest:        digestHex(wav),
		ByteCount:     int64(len(wav)),
		SampleRate:    48000,
		Channels:      channels,
		BitsPerSample: 16,
		FrameCount:    frames,
		DurationMS:    frames * 1000 / 48000,
		PeakDBFS:      legacyPeak,
		RMSDBFS:       legacyRMS,
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(dir, "metadata.json"), asset, 0o640); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}
	return asset
}

func digestHex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func approxEqual(t *testing.T, got, want float64, tolerance float64, label string) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %.4f, want %.4f (±%.4f)", label, got, want, tolerance)
	}
}

func TestGetSourceMetaHealsLegacyMeasurements(t *testing.T) {
	repo := newTestRepository(t)
	// Anti-phase stereo: legacy mono fold cancels to the -120 silence floor.
	wav := stereoWAVBytes(t, 480, 0.5, -0.5)
	writeLegacySource(t, repo, "antiphase", wav, -120, -120, 2, 480)

	healed, err := repo.GetSourceMeta("antiphase")
	if err != nil {
		t.Fatalf("heal on read: %v", err)
	}
	want := 20 * math.Log10(0.5)
	approxEqual(t, healed.PeakDBFS, want, 0.02, "healed peak")
	approxEqual(t, healed.RMSDBFS, want, 0.02, "healed rms")
	if healed.MeasurementsVersion != domain.MeasurementsVersionV2 {
		t.Fatalf("healed version = %d, want %d", healed.MeasurementsVersion, domain.MeasurementsVersionV2)
	}

	// The correction must be durably committed to metadata.
	var persisted domain.SourceAsset
	if err := readJSON(repo.path("sources", "antiphase", "metadata.json"), &persisted); err != nil {
		t.Fatalf("read persisted metadata: %v", err)
	}
	if persisted.MeasurementsVersion != domain.MeasurementsVersionV2 || math.Abs(persisted.PeakDBFS-want) > 0.02 {
		t.Fatalf("persisted metadata not corrected: %+v", persisted)
	}
	again, err := repo.GetSourceMeta("antiphase")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	approxEqual(t, again.PeakDBFS, want, 0.02, "repeat read peak")
}

func TestBackfillRepairsMixedLegacyAndCurrentSources(t *testing.T) {
	repo := newTestRepository(t)
	writeLegacySource(t, repo, "legacy_antiphase", stereoWAVBytes(t, 240, 0.5, -0.5), -120, -120, 2, 240)
	writeLegacySource(t, repo, "legacy_unbalanced", stereoWAVBytes(t, 240, 0.4, 0.1),
		20*math.Log10(0.25), 20*math.Log10(0.25), 2, 240)
	current := writeLegacySource(t, repo, "current", stereoWAVBytes(t, 240, 0.3, 0.3),
		20*math.Log10(0.3), 20*math.Log10(0.3), 2, 240)
	current.MeasurementsVersion = domain.MeasurementsVersionV2
	if err := writeJSONAtomic(repo.path("sources", "current", "metadata.json"), current, 0o640); err != nil {
		t.Fatalf("mark current source: %v", err)
	}

	repaired, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if repaired != 2 {
		t.Fatalf("backfill repaired %d sources, want 2", repaired)
	}

	anti, err := repo.GetSourceMeta("legacy_antiphase")
	if err != nil {
		t.Fatalf("read repaired antiphase: %v", err)
	}
	approxEqual(t, anti.PeakDBFS, 20*math.Log10(0.5), 0.02, "antiphase peak")

	unbalanced, err := repo.GetSourceMeta("legacy_unbalanced")
	if err != nil {
		t.Fatalf("read repaired unbalanced: %v", err)
	}
	approxEqual(t, unbalanced.PeakDBFS, 20*math.Log10(0.4), 0.02, "unbalanced peak")
	wantRMS := 20 * math.Log10(math.Sqrt((0.4*0.4+0.1*0.1)/2))
	approxEqual(t, unbalanced.RMSDBFS, wantRMS, 0.02, "unbalanced rms")

	// Re-running must be cheap and idempotent.
	again, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil || again != 0 {
		t.Fatalf("second backfill = (%d, %v), want (0, nil)", again, err)
	}
}

func TestBackfillResumesAfterInterruption(t *testing.T) {
	repo := newTestRepository(t)
	const total = 40
	for index := 0; index < total; index++ {
		id := "legacy_" + string(rune('a'+index/26)) + string(rune('a'+index%26))
		writeLegacySource(t, repo, id, stereoWAVBytes(t, 120, 0.5, -0.5), -120, -120, 2, 120)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var firstRepaired int
	var firstErr error
	go func() {
		firstRepaired, firstErr = repo.BackfillSourceMeasurements(ctx)
		close(done)
	}()
	cancel()
	<-done

	// Whether or not the first run finished, every on-disk record must be
	// either fully legacy or fully corrected: no half-written metadata.
	entries, err := os.ReadDir(repo.path("sources"))
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	seenCorrected := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		payload, readErr := os.ReadFile(repo.path("sources", entry.Name(), "metadata.json"))
		if readErr != nil {
			t.Fatalf("read %s metadata after interruption: %v", entry.Name(), readErr)
		}
		if !json.Valid(payload) {
			t.Fatalf("source %s has invalid metadata after interruption", entry.Name())
		}
		var asset domain.SourceAsset
		if err := json.Unmarshal(payload, &asset); err != nil {
			t.Fatalf("decode %s metadata: %v", entry.Name(), err)
		}
		switch asset.MeasurementsVersion {
		case 0:
			if asset.PeakDBFS != -120 {
				t.Fatalf("legacy record %s unexpectedly modified", entry.Name())
			}
		case domain.MeasurementsVersionV2:
			seenCorrected++
			approxEqual(t, asset.PeakDBFS, 20*math.Log10(0.5), 0.02, entry.Name()+" peak")
		default:
			t.Fatalf("source %s has unknown measurements version %d", entry.Name(), asset.MeasurementsVersion)
		}
		if leftovers, _ := filepath.Glob(repo.path("sources", entry.Name(), ".write-*")); len(leftovers) > 0 {
			t.Fatalf("temporary metadata files left behind in %s: %v", entry.Name(), leftovers)
		}
	}
	if firstErr != nil && seenCorrected >= total {
		t.Fatalf("backfill reported error %v despite full completion", firstErr)
	}
	if firstErr == nil && firstRepaired != total {
		t.Fatalf("completed backfill repaired %d, want %d", firstRepaired, total)
	}

	// A restart must converge the remaining records without touching media.
	remaining, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil {
		t.Fatalf("resume backfill: %v", err)
	}
	if remaining != total-seenCorrected {
		t.Fatalf("resume repaired %d, want %d", remaining, total-seenCorrected)
	}
	finalRepaired, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil || finalRepaired != 0 {
		t.Fatalf("final backfill = (%d, %v), want (0, nil)", finalRepaired, err)
	}
}

func TestGetSourceMetaDoesNotReadMediaForCurrentRecords(t *testing.T) {
	repo := newTestRepository(t)
	wav := stereoWAVBytes(t, 240, 0.5, -0.5)
	asset := writeLegacySource(t, repo, "current", wav, 20*math.Log10(0.5), 20*math.Log10(0.5), 2, 240)
	asset.MeasurementsVersion = domain.MeasurementsVersionV2
	if err := writeJSONAtomic(repo.path("sources", "current", "metadata.json"), asset, 0o640); err != nil {
		t.Fatalf("write current metadata: %v", err)
	}
	mediaPath := repo.path("sources", "current", "source.wav")
	if err := os.Chmod(mediaPath, 0o000); err != nil {
		t.Fatalf("hide media: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(mediaPath, 0o640) })

	// Detail-page refresh must stay metadata-only even with the media
	// unreadable, so repeated reads never scale with recording length.
	for attempt := 0; attempt < 3; attempt++ {
		got, err := repo.GetSourceMeta("current")
		if err != nil {
			t.Fatalf("current source read %d failed: %v", attempt, err)
		}
		if got.MeasurementsVersion != domain.MeasurementsVersionV2 {
			t.Fatalf("read %d returned version %d", attempt, got.MeasurementsVersion)
		}
	}
}

func TestHealingIsDeterministic(t *testing.T) {
	repo := newTestRepository(t)
	wav := stereoWAVBytes(t, 360, 0.4, 0.1)
	writeLegacySource(t, repo, "unbalanced", wav, 20*math.Log10(0.25), 20*math.Log10(0.25), 2, 360)
	first, err := repo.GetSourceMeta("unbalanced")
	if err != nil {
		t.Fatalf("first heal: %v", err)
	}
	second, err := repo.GetSourceMeta("unbalanced")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first.PeakDBFS != second.PeakDBFS || first.RMSDBFS != second.RMSDBFS {
		t.Fatalf("healing not deterministic: %+v vs %+v", first, second)
	}
}

func TestConcurrentReadsHealLegacySourceOnce(t *testing.T) {
	repo := newTestRepository(t)
	wav := stereoWAVBytes(t, 480, 0.5, -0.5)
	writeLegacySource(t, repo, "contended", wav, -120, -120, 2, 480)

	const readers = 16
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			asset, err := repo.GetSourceMeta("contended")
			if err != nil {
				errs <- err
				return
			}
			want := 20 * math.Log10(0.5)
			if math.Abs(asset.PeakDBFS-want) > 0.02 || asset.MeasurementsVersion != domain.MeasurementsVersionV2 {
				errs <- fmt.Errorf("concurrent read returned peak=%.3f version=%d", asset.PeakDBFS, asset.MeasurementsVersion)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var persisted domain.SourceAsset
	if err := readJSON(repo.path("sources", "contended", "metadata.json"), &persisted); err != nil {
		t.Fatalf("read persisted metadata: %v", err)
	}
	if persisted.MeasurementsVersion != domain.MeasurementsVersionV2 {
		t.Fatalf("persisted version = %d, want v2", persisted.MeasurementsVersion)
	}
}
