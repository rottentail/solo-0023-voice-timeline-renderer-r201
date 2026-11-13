package storage

import (
	"errors"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
)

func (repo *Repository) SaveJob(job domain.RenderJob) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", job.ID+".json")
	if err := writeJSONAtomic(path, job, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render job", err)
	}
	return nil
}

func (repo *Repository) GetJob(id string) (domain.RenderJob, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var job domain.RenderJob
	if err := readJSON(repo.path("jobs", id+".json"), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderJob{}, domain.E(domain.KindNotFound, "render_not_found", "render_id", "render job does not exist")
		}
		return domain.RenderJob{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render job", err)
	}
	return job, nil
}

// CommitRenderArtifacts publishes the master, every stem, and the receipt as
// one atomic delivery: all files are staged in a temporary directory and a
// single rename makes them visible. If any stem fails to write, the staging
// directory is removed and nothing of the delivery is published.
func (repo *Repository) CommitRenderArtifacts(id string, wav []byte, stems []domain.StemDelivery, receipt domain.RenderReceipt) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	final := repo.path("renders", id)
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	temp, err := os.MkdirTemp(repo.path("tmp"), "render-*")
	if err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create render staging directory", err)
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	if err := writeBytes(filepath.Join(temp, "output.wav"), wav, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render output", err)
	}
	if len(stems) > 0 {
		if err := os.MkdirAll(filepath.Join(temp, "stems"), 0o750); err != nil {
			return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create stem staging directory", err)
		}
		for _, stem := range stems {
			if !safeStemName(stem.GroupID) {
				return domain.E(domain.KindInvalid, "invalid_delivery_group", "group_id", "delivery group identifier is not a safe file name")
			}
			if err := writeBytes(filepath.Join(temp, "stems", stem.GroupID+".wav"), stem.WAV, 0o640); err != nil {
				return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write stem output", err)
			}
		}
	}
	if err := writeJSONAtomic(filepath.Join(temp, "receipt.json"), receipt, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render receipt", err)
	}
	if err := os.Rename(temp, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit render artifacts", err)
	}
	if err := syncDirectory(repo.path("renders")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render index", err)
	}
	return nil
}

// safeStemName rejects group identifiers that could escape the stems
// directory. Timeline validation already enforces this shape; the storage
// boundary rechecks it before building file paths.
func safeStemName(groupID string) bool {
	if groupID == "" {
		return false
	}
	for _, r := range groupID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (repo *Repository) GetRenderOutput(id string) ([]byte, domain.RenderReceipt, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var receipt domain.RenderReceipt
	if err := readJSON(repo.path("renders", id, "receipt.json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render receipt", err)
	}
	payload, err := os.ReadFile(repo.path("renders", id, "output.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render output", err)
	}
	return payload, receipt, nil
}

// GetRenderStem reads one published stem of a completed render.
func (repo *Repository) GetRenderStem(id string, groupID string) ([]byte, error) {
	if !safeStemName(groupID) {
		return nil, domain.E(domain.KindInvalid, "invalid_delivery_group", "group_id", "delivery group identifier is not a safe file name")
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	payload, err := os.ReadFile(repo.path("renders", id, "stems", groupID+".wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.E(domain.KindNotFound, "stem_not_found", "group_id", "render has no published stem for this delivery group")
		}
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read stem output", err)
	}
	return payload, nil
}
