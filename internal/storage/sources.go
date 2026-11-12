package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func (repo *Repository) SaveSource(asset domain.SourceAsset, wav []byte) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	final := repo.path("sources", asset.ID)
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "source_conflict", "source_id", "source identifier already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect source destination", err)
	}
	temp, err := os.MkdirTemp(repo.path("tmp"), "source-*")
	if err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create source staging directory", err)
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	if err := writeBytes(filepath.Join(temp, "source.wav"), wav, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write source media", err)
	}
	asset.MeasurementsVersion = domain.MeasurementsVersionV2
	if err := writeJSONAtomic(filepath.Join(temp, "metadata.json"), asset, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write source metadata", err)
	}
	if err := os.Rename(temp, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit source", err)
	}
	if err := syncDirectory(repo.path("sources")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync source index", err)
	}
	return nil
}

func (repo *Repository) GetSourceMeta(id string) (domain.SourceAsset, error) {
	repo.mu.RLock()
	asset, err := repo.readSourceMetaLocked(id)
	repo.mu.RUnlock()
	if err != nil {
		return domain.SourceAsset{}, err
	}
	if asset.MeasurementsVersion >= domain.MeasurementsVersionV2 {
		return asset, nil
	}
	// Legacy record: take the exclusive lock for the recompute and atomic
	// metadata rewrite so concurrent reads of the same source cannot heal it
	// in parallel.
	repo.mu.Lock()
	defer repo.mu.Unlock()
	// Re-read after acquiring the write lock: another goroutine may have
	// healed this source while we waited.
	current, err := repo.readSourceMetaLocked(id)
	if err != nil {
		return domain.SourceAsset{}, err
	}
	if current.MeasurementsVersion >= domain.MeasurementsVersionV2 {
		return current, nil
	}
	return repo.healSourceMeasurementsLocked(id, current)
}

// readSourceMetaLocked loads metadata without touching the media payload so
// repeated detail-page reads stay constant time regardless of recording
// length. Callers must hold at least the repository read lock.
func (repo *Repository) readSourceMetaLocked(id string) (domain.SourceAsset, error) {
	var asset domain.SourceAsset
	if err := readJSON(repo.path("sources", id, "metadata.json"), &asset); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.SourceAsset{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source does not exist")
		}
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source metadata", err)
	}
	return asset, nil
}

// healSourceMeasurementsLocked recomputes legacy mid-downmix measurements
// from the immutable stored WAV and commits corrected metadata atomically.
// The media bytes are verified against the recorded digest first, so the new
// numbers always describe the exact stored recording. A failed commit leaves
// the previous metadata in place; the next read simply retries.
func (repo *Repository) healSourceMeasurementsLocked(id string, asset domain.SourceAsset) (domain.SourceAsset, error) {
	payload, err := os.ReadFile(repo.path("sources", id, "source.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.SourceAsset{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source media does not exist")
		}
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot load source media", err)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != asset.Digest {
		return domain.SourceAsset{}, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source digest does not match metadata")
	}
	clip, _, err := audiocodec.DecodeWAV(payload)
	if err != nil {
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "source_integrity_error", "source_id", "cannot decode stored source", err)
	}
	measured := audiocodec.MeasureClip(clip)
	asset.PeakDBFS = measured.PeakDBFS
	asset.RMSDBFS = measured.RMSDBFS
	asset.MeasurementsVersion = domain.MeasurementsVersionV2
	if err := writeJSONAtomic(repo.path("sources", id, "metadata.json"), asset, 0o640); err != nil {
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot rewrite source measurements", err)
	}
	return asset, nil
}

// BackfillSourceMeasurements incrementally recomputes legacy source
// measurements from the stored WAV files without requiring the directory to
// be emptied or re-registered. Each corrected metadata file is committed with
// an atomic same-directory rename, so a restart at any point leaves every
// record either fully old or fully new; sources already marked current are
// skipped, making repeated runs cheap and resumable. Work proceeds in small
// batches and releases the lock between them so other production workflows
// keep being served.
func (repo *Repository) BackfillSourceMeasurements(ctx context.Context) (int, error) {
	const batchSize = 8
	repaired := 0
	for {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		repo.mu.Lock()
		entries, err := os.ReadDir(repo.path("sources"))
		if err != nil {
			repo.mu.Unlock()
			return repaired, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot list stored sources", err)
		}
		changed := 0
		for _, entry := range entries {
			if changed >= batchSize {
				break
			}
			if !entry.IsDir() {
				continue
			}
			id := entry.Name()
			asset, readErr := repo.readSourceMetaLocked(id)
			if readErr != nil {
				repo.mu.Unlock()
				return repaired, readErr
			}
			if asset.MeasurementsVersion >= domain.MeasurementsVersionV2 {
				continue
			}
			if _, healErr := repo.healSourceMeasurementsLocked(id, asset); healErr != nil {
				repo.mu.Unlock()
				return repaired, healErr
			}
			repaired++
			changed++
		}
		repo.mu.Unlock()
		if changed == 0 {
			return repaired, nil
		}
		slog.Info("source measurement backfill progress", "repaired", repaired)
	}
}

func (repo *Repository) GetSource(id string) (domain.SourceAsset, []byte, error) {
	asset, err := repo.GetSourceMeta(id)
	if err != nil {
		return domain.SourceAsset{}, nil, err
	}
	repo.mu.RLock()
	payload, readErr := os.ReadFile(repo.path("sources", id, "source.wav"))
	repo.mu.RUnlock()
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return domain.SourceAsset{}, nil, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source media does not exist")
		}
		return domain.SourceAsset{}, nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source media", readErr)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != asset.Digest {
		return domain.SourceAsset{}, nil, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source digest does not match metadata")
	}
	return asset, payload, nil
}
