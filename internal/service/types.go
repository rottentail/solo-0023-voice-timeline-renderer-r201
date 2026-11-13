package service

import (
	"context"
	"sync"

	"voice-timeline-renderer/internal/domain"
)

type Repository interface {
	SaveSource(domain.SourceAsset, []byte) error
	GetSourceMeta(string) (domain.SourceAsset, error)
	GetSource(string) (domain.SourceAsset, []byte, error)
	SaveTimeline(domain.Timeline) error
	GetTimeline(string) (domain.Timeline, error)
	SaveJob(domain.RenderJob) error
	GetJob(string) (domain.RenderJob, error)
	CommitRenderArtifacts(string, []byte, []domain.StemDelivery, domain.RenderReceipt) error
	GetRenderOutput(string) ([]byte, domain.RenderReceipt, error)
	GetRenderStem(string, string) ([]byte, error)
}

type Config struct {
	MaxSourceBytes    int64
	MaxSpans          int
	MaxOutputFrames   int64
	MaxDeliveryGroups int
}

func DefaultConfig() Config {
	return Config{
		MaxSourceBytes:    32 << 20,
		MaxSpans:          64,
		MaxOutputFrames:   48000 * 180,
		MaxDeliveryGroups: 32,
	}
}

type Service struct {
	repo   Repository
	config Config
	mu     sync.Mutex
}

func New(repo Repository, config Config) *Service {
	defaults := DefaultConfig()
	if config.MaxSourceBytes <= 0 {
		config.MaxSourceBytes = defaults.MaxSourceBytes
	}
	if config.MaxSpans <= 0 {
		config.MaxSpans = defaults.MaxSpans
	}
	if config.MaxOutputFrames <= 0 {
		config.MaxOutputFrames = defaults.MaxOutputFrames
	}
	if config.MaxDeliveryGroups <= 0 {
		config.MaxDeliveryGroups = defaults.MaxDeliveryGroups
	}
	return &Service{repo: repo, config: config}
}

type SourceWriteRequest struct {
	Name      string `json:"name"`
	WAVBase64 string `json:"wav_base64"`
}

type TimelineWriteRequest struct {
	Name           string                 `json:"name"`
	SampleRate     int                    `json:"sample_rate"`
	OutputChannels int                    `json:"output_channels"`
	TargetPeakDBFS float64                `json:"target_peak_dbfs"`
	DeliveryGroups []domain.DeliveryGroup `json:"delivery_groups"`
	Spans          []domain.TimelineSpan  `json:"spans"`
}

type RenderWriteRequest struct {
	TimelineID string `json:"timeline_id"`
}

type RenderResult struct {
	Job domain.RenderJob
	WAV []byte
}

func (s *Service) GetSource(ctx context.Context, id string) (domain.SourceAsset, error) {
	if err := ctx.Err(); err != nil {
		return domain.SourceAsset{}, err
	}
	return s.repo.GetSourceMeta(id)
}

func (s *Service) GetTimeline(ctx context.Context, id string) (domain.Timeline, error) {
	if err := ctx.Err(); err != nil {
		return domain.Timeline{}, err
	}
	return s.repo.GetTimeline(id)
}

func (s *Service) GetRender(ctx context.Context, id string) (domain.RenderJob, error) {
	if err := ctx.Err(); err != nil {
		return domain.RenderJob{}, err
	}
	return s.repo.GetJob(id)
}

func (s *Service) GetReceipt(ctx context.Context, id string) (domain.RenderReceipt, error) {
	job, err := s.GetRender(ctx, id)
	if err != nil {
		return domain.RenderReceipt{}, err
	}
	if job.State != domain.RenderCompleted {
		return domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render receipt is available only after completion")
	}
	_, receipt, err := s.repo.GetRenderOutput(id)
	return receipt, err
}

func (s *Service) GetOutput(ctx context.Context, id string) ([]byte, domain.RenderReceipt, error) {
	job, err := s.GetRender(ctx, id)
	if err != nil {
		return nil, domain.RenderReceipt{}, err
	}
	if job.State != domain.RenderCompleted {
		return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is available only after completion")
	}
	return s.repo.GetRenderOutput(id)
}

// GetStem returns the encoded stem of one delivery group of a completed
// render. A render that failed or is still active never exposes stems, so a
// partially committed delivery can never be mistaken for a complete one.
func (s *Service) GetStem(ctx context.Context, id string, groupID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateGroupID("group", groupID); err != nil {
		return nil, err
	}
	job, err := s.GetRender(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.State != domain.RenderCompleted {
		return nil, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render stems are available only after completion")
	}
	return s.repo.GetRenderStem(id, groupID)
}
