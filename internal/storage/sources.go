package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	defer repo.mu.RUnlock()
	var asset domain.SourceAsset
	if err := readJSON(repo.path("sources", id, "metadata.json"), &asset); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.SourceAsset{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source does not exist")
		}
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source metadata", err)
	}
	return repo.sourceDisplayAsset(asset)
}

func (repo *Repository) sourceDisplayAsset(asset domain.SourceAsset) (domain.SourceAsset, error) {
	if asset.Channels != 2 {
		return asset, nil
	}
	payload, err := os.ReadFile(repo.path("sources", asset.ID, "source.wav"))
	if err != nil {
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot load source monitor", err)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != asset.Digest {
		return domain.SourceAsset{}, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source digest does not match metadata")
	}
	clip, _, err := audiocodec.DecodeWAV(payload)
	if err != nil {
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "source_integrity_error", "source_id", "cannot decode stored source", err)
	}
	frames := clip.FrameCount()
	monitor := make([]float32, frames)
	for frame := int64(0); frame < frames; frame++ {
		start := int(frame) * clip.Channels
		left := clip.Samples[start]
		right := clip.Samples[start+1]
		monitor[frame] = (left + right) * 0.5
	}
	measured := audiocodec.Measure(monitor)
	asset.PeakDBFS = measured.PeakDBFS
	asset.RMSDBFS = measured.RMSDBFS
	return asset, nil
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
