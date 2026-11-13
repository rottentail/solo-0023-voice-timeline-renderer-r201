package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

// runStemDelivery verifies grouped stem delivery: one render produces a
// time-aligned master plus one stem per delivery group, the stems share the
// master normalization gain, their unquantized sum reconstructs the master,
// cross-group junctions never fade another group, and a failed commit
// publishes nothing.
func runStemDelivery(ctx context.Context, harness *Harness) error {
	sources := []struct {
		id        string
		name      string
		channels  int
		duration  int
		frequency float64
		amplitude float64
	}{
		{"source_stem_vox1", "narration one", 1, 500, 220, 0.5},
		{"source_stem_vox2", "narration two", 1, 500, 440, 0.25},
		{"source_stem_mus1", "score one", 2, 600, 330, 0.4},
		{"source_stem_mus2", "score two", 2, 400, 550, 0.15},
	}
	for _, source := range sources {
		wav, err := sineWAV(48000, source.channels, source.duration, source.frequency, source.amplitude)
		if err != nil {
			return err
		}
		if _, status, err := harness.createSource(ctx, source.id, source.name, wav); err != nil || status != 201 {
			return fmt.Errorf("source %s setup failed: status=%d err=%v", source.id, status, err)
		}
	}

	spans := []domain.TimelineSpan{
		{SourceID: "source_stem_vox1", SourceStartFrame: 0, SourceEndFrame: 24000, GainDB: 0, CrossfadeFrames: 0, GroupID: "vocals"},
		{SourceID: "source_stem_vox2", SourceStartFrame: 0, SourceEndFrame: 24000, GainDB: 0, CrossfadeFrames: 2400, GroupID: "vocals"},
		{SourceID: "source_stem_mus1", SourceStartFrame: 0, SourceEndFrame: 28800, GainDB: 0, CrossfadeFrames: 2400, GroupID: "music"},
		{SourceID: "source_stem_mus2", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: 0, CrossfadeFrames: 2400, GroupID: "music"},
	}
	groups := []domain.DeliveryGroup{
		{ID: "vocals", Label: "narration"},
		{ID: "music", Label: "score"},
	}
	plan, status, err := harness.putTimeline(ctx, "timeline_stem_01", service.TimelineWriteRequest{
		Name:           "grouped delivery",
		SampleRate:     48000,
		OutputChannels: 2,
		TargetPeakDBFS: -1,
		DeliveryGroups: groups,
		Spans:          spans,
	})
	if err != nil {
		return err
	}
	if status != 201 || plan.State != "ready" {
		return fmt.Errorf("grouped timeline returned status=%d state=%s", status, plan.State)
	}
	const wantFrames = 88800
	if plan.OutputFrames != wantFrames {
		return fmt.Errorf("grouped timeline frames=%d, want %d", plan.OutputFrames, wantFrames)
	}
	readBack, status, err := harness.getTimeline(ctx, plan.ID)
	if err != nil {
		return err
	}
	if status != 200 || len(readBack.DeliveryGroups) != 2 ||
		readBack.Spans[0].GroupID != "vocals" || readBack.Spans[2].GroupID != "music" {
		return fmt.Errorf("timeline read-back lost delivery groups: status=%d groups=%+v", status, readBack.DeliveryGroups)
	}
	repeat, status, err := harness.putTimeline(ctx, "timeline_stem_01", service.TimelineWriteRequest{
		Name:           "grouped delivery",
		SampleRate:     48000,
		OutputChannels: 2,
		TargetPeakDBFS: -1,
		DeliveryGroups: groups,
		Spans:          spans,
	})
	if err != nil {
		return err
	}
	if status != 200 || repeat.OutputFrames != plan.OutputFrames {
		return fmt.Errorf("grouped timeline re-write not idempotent: status=%d", status)
	}

	if err := checkGroupValidation(ctx, harness); err != nil {
		return err
	}

	job, status, err := harness.createRender(ctx, "render_stem_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted || job.Output == nil {
		return fmt.Errorf("grouped render returned status=%d state=%s", status, job.State)
	}
	if len(job.Stems) != 2 || job.Stems[0].GroupID != "vocals" || job.Stems[1].GroupID != "music" {
		return fmt.Errorf("completed job does not list delivered stems: %+v", job.Stems)
	}

	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 || receipt.Delivery == nil {
		return fmt.Errorf("grouped render receipt misses the delivery section: status=%d", status)
	}
	if receipt.SchemaVersion != 2 {
		return fmt.Errorf("grouped receipt schema_version=%d, want 2", receipt.SchemaVersion)
	}
	if receipt.Delivery.SharedGainDB != receipt.Normalization.AppliedGainDB {
		return fmt.Errorf("stems do not share the master gain: shared=%.6f master=%.6f",
			receipt.Delivery.SharedGainDB, receipt.Normalization.AppliedGainDB)
	}
	if receipt.Output.FrameCount != wantFrames {
		return fmt.Errorf("master frames=%d, want %d", receipt.Output.FrameCount, wantFrames)
	}
	if len(receipt.Spans) != 4 || receipt.Spans[0].GroupID != "vocals" || receipt.Spans[3].GroupID != "music" {
		return fmt.Errorf("receipt spans lost group membership: %+v", receipt.Spans)
	}
	stems := receipt.Delivery.Stems
	if len(stems) != 2 {
		return fmt.Errorf("receipt lists %d stems, want 2", len(stems))
	}
	for _, stem := range stems {
		if stem.FrameCount != receipt.Output.FrameCount {
			return fmt.Errorf("stem %s is not time-aligned: frames=%d master=%d", stem.GroupID, stem.FrameCount, receipt.Output.FrameCount)
		}
	}
	if len(stems[0].SourceIDs) != 2 || stems[0].SourceIDs[0] != "source_stem_vox1" || stems[0].SourceIDs[1] != "source_stem_vox2" {
		return fmt.Errorf("vocals stem sources are wrong: %+v", stems[0].SourceIDs)
	}
	if len(stems[1].SourceIDs) != 2 || stems[1].SourceIDs[0] != "source_stem_mus1" || stems[1].SourceIDs[1] != "source_stem_mus2" {
		return fmt.Errorf("music stem sources are wrong: %+v", stems[1].SourceIDs)
	}
	quantization := receipt.Delivery.Quantization
	if quantization.Format != "pcm_s16le" || quantization.Dither != "none" || quantization.LSB <= 0 {
		return fmt.Errorf("receipt does not explain the applied quantization: %+v", quantization)
	}
	if receipt.Delivery.Reconstruction.MaxDeviationLSB <= 0 {
		return fmt.Errorf("receipt does not bound the 16-bit reconstruction deviation: %+v", receipt.Delivery.Reconstruction)
	}
	for _, warning := range receipt.Warnings {
		if strings.Contains(warning, "saturated") {
			return fmt.Errorf("unexpected saturation warning: %s", warning)
		}
	}

	masterWAV, status, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	vocalsWAV, status, err := harness.getStem(ctx, job.ID, "vocals")
	if err != nil {
		return err
	}
	musicWAV, status, err := harness.getStem(ctx, job.ID, "music")
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("stem download returned status %d", status)
	}
	badStatus, badBody, err := harness.requestJSON(ctx, "GET", "/v1/renders/"+job.ID+"/stems/bad.group", nil)
	if err != nil {
		return err
	}
	if badStatus != 422 || failureCode(badBody) != "invalid_delivery_group" {
		return fmt.Errorf("malformed stem group returned status=%d code=%s", badStatus, failureCode(badBody))
	}
	if _, status, err := harness.getStem(ctx, job.ID, "effects"); err == nil || status != 404 {
		return fmt.Errorf("undeclared group returned a stem: status=%d err=%v", status, err)
	}
	if digest := sha256.Sum256(masterWAV); hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		return fmt.Errorf("master digest does not match receipt")
	}
	if digest := sha256.Sum256(vocalsWAV); hex.EncodeToString(digest[:]) != stems[0].Digest {
		return fmt.Errorf("vocals stem digest does not match receipt")
	}
	if digest := sha256.Sum256(musicWAV); hex.EncodeToString(digest[:]) != stems[1].Digest {
		return fmt.Errorf("music stem digest does not match receipt")
	}

	master, masterInfo, err := audiocodec.DecodeWAV(masterWAV)
	if err != nil {
		return err
	}
	vocals, _, err := audiocodec.DecodeWAV(vocalsWAV)
	if err != nil {
		return err
	}
	music, _, err := audiocodec.DecodeWAV(musicWAV)
	if err != nil {
		return err
	}
	if masterInfo.FrameCount != wantFrames || vocals.FrameCount() != wantFrames || music.FrameCount() != wantFrames {
		return fmt.Errorf("deliveries are not time-aligned: master=%d vocals=%d music=%d",
			masterInfo.FrameCount, vocals.FrameCount(), music.FrameCount())
	}
	if measured := audiocodec.Measure(master.Samples); math.Abs(measured.PeakDBFS-(-1)) > 0.25 {
		return fmt.Errorf("master peak %.3f dBFS does not meet -1 dBFS target", measured.PeakDBFS)
	}

	bound := receipt.Delivery.Reconstruction.MaxDeviationLSB * quantization.LSB
	worst := 0.0
	for index := range master.Samples {
		deviation := math.Abs(float64(master.Samples[index]) - float64(vocals.Samples[index]) - float64(music.Samples[index]))
		if deviation > worst {
			worst = deviation
		}
	}
	if worst > bound {
		return fmt.Errorf("stem sum deviates from master by %.8f, above the explained quantization bound %.8f", worst, bound)
	}

	// Cross-group junction at frames [43200, 45600): neither group may fade.
	if ratio := windowPeakRatio(music, 43200, 43400, 50000, 50200); ratio < 0.8 {
		return fmt.Errorf("cross-group junction faded the incoming music stem: ratio=%.3f", ratio)
	}
	if ratio := windowPeakRatio(vocals, 45400, 45600, 40000, 40200); ratio < 0.8 {
		return fmt.Errorf("cross-group junction faded the outgoing vocals stem: ratio=%.3f", ratio)
	}
	// Same-group junctions keep the equal-power crossfade.
	if ratio := windowPeakRatio(vocals, 23900, 24000, 21000, 21600); ratio > 0.75 {
		return fmt.Errorf("same-group junction lost its fade-out: ratio=%.3f", ratio)
	}
	if ratio := windowPeakRatio(music, 71900, 72000, 69000, 69600); ratio > 0.8 {
		return fmt.Errorf("same-group music junction lost its fade-out: ratio=%.3f", ratio)
	}

	if err := checkLegacyRenderUnchanged(ctx, harness, spans); err != nil {
		return err
	}
	if err := checkStemCommitFailure(ctx); err != nil {
		return err
	}
	return checkStemCommitAtomicity()
}

// checkGroupValidation rejects malformed delivery group declarations.
func checkGroupValidation(ctx context.Context, harness *Harness) error {
	base := service.TimelineWriteRequest{
		Name:           "invalid grouping",
		SampleRate:     48000,
		OutputChannels: 2,
		TargetPeakDBFS: -1,
		DeliveryGroups: []domain.DeliveryGroup{{ID: "vocals"}},
	}
	cases := []struct {
		id     string
		mutate func(*service.TimelineWriteRequest)
	}{
		{"timeline_stem_bad1", func(request *service.TimelineWriteRequest) {
			request.Spans = []domain.TimelineSpan{
				{SourceID: "source_stem_vox1", SourceStartFrame: 0, SourceEndFrame: 24000, GroupID: "missing"},
			}
		}},
		{"timeline_stem_bad2", func(request *service.TimelineWriteRequest) {
			request.Spans = []domain.TimelineSpan{
				{SourceID: "source_stem_vox1", SourceStartFrame: 0, SourceEndFrame: 24000},
			}
		}},
		{"timeline_stem_bad3", func(request *service.TimelineWriteRequest) {
			request.DeliveryGroups = append(request.DeliveryGroups, domain.DeliveryGroup{ID: "unused"})
			request.Spans = []domain.TimelineSpan{
				{SourceID: "source_stem_vox1", SourceStartFrame: 0, SourceEndFrame: 24000, GroupID: "vocals"},
			}
		}},
		{"timeline_stem_bad4", func(request *service.TimelineWriteRequest) {
			request.DeliveryGroups = nil
			request.Spans = []domain.TimelineSpan{
				{SourceID: "source_stem_vox1", SourceStartFrame: 0, SourceEndFrame: 24000, GroupID: "vocals"},
			}
		}},
	}
	for _, testCase := range cases {
		request := base
		testCase.mutate(&request)
		status, body, err := harness.requestJSON(ctx, "PUT", "/v1/timelines/"+testCase.id, request)
		if err != nil {
			return err
		}
		if status != 422 || failureCode(body) != "invalid_delivery_group" {
			return fmt.Errorf("%s returned status=%d code=%s, want 422 invalid_delivery_group", testCase.id, status, failureCode(body))
		}
		if _, status, err := harness.getTimeline(ctx, testCase.id); err != nil || status != 404 {
			return fmt.Errorf("rejected timeline %s became visible: status=%d err=%v", testCase.id, status, err)
		}
	}
	return nil
}

// checkLegacyRenderUnchanged confirms that timelines without delivery groups
// keep the original deterministic PCM path and expose no stem resources.
func checkLegacyRenderUnchanged(ctx context.Context, harness *Harness, grouped []domain.TimelineSpan) error {
	legacySpans := make([]domain.TimelineSpan, len(grouped))
	for index, span := range grouped {
		span.GroupID = ""
		legacySpans[index] = span
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_stem_legacy", "ungrouped assembly", 48000, 2, -1, legacySpans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("legacy timeline returned status %d", status)
	}
	first, status, err := harness.createRender(ctx, "render_stem_legacy_a", plan.ID)
	if err != nil {
		return err
	}
	second, status, err := harness.createRender(ctx, "render_stem_legacy_b", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || first.State != domain.RenderCompleted || second.State != domain.RenderCompleted {
		return fmt.Errorf("legacy renders returned status=%d states=%s/%s", status, first.State, second.State)
	}
	receipt, status, err := harness.getReceipt(ctx, first.ID)
	if err != nil {
		return err
	}
	if status != 200 || receipt.Delivery != nil || receipt.SchemaVersion != 1 {
		return fmt.Errorf("ungrouped render gained a delivery section: status=%d version=%d", status, receipt.SchemaVersion)
	}
	firstWAV, _, err := harness.getOutput(ctx, first.ID)
	if err != nil {
		return err
	}
	secondWAV, _, err := harness.getOutput(ctx, second.ID)
	if err != nil {
		return err
	}
	if sha256.Sum256(firstWAV) != sha256.Sum256(secondWAV) {
		return fmt.Errorf("ungrouped render is not deterministic")
	}
	if _, status, err := harness.getStem(ctx, first.ID, "vocals"); err == nil || status != 404 {
		return fmt.Errorf("ungrouped render exposed a stem: status=%d err=%v", status, err)
	}
	return nil
}

// commitFailingRepository fails the artifact commit so the check can verify
// that no partial delivery becomes visible.
type commitFailingRepository struct {
	service.Repository
	cause error
}

func (repo commitFailingRepository) CommitRenderArtifacts(string, []byte, []domain.StemDelivery, domain.RenderReceipt) error {
	return repo.cause
}

// checkStemCommitFailure drives a grouped render whose commit fails and
// verifies the job fails terminally and nothing is downloadable.
func checkStemCommitFailure(ctx context.Context) error {
	tempDir, err := os.MkdirTemp("", "voice-timeline-check-fail-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()
	repo, err := storage.NewRepository(tempDir)
	if err != nil {
		return err
	}
	failing := commitFailingRepository{
		Repository: repo,
		cause:      domain.E(domain.KindInternal, "commit_simulated", "", "simulated commit failure"),
	}
	harness := NewHarnessWithRepository(failing)
	defer harness.Close()

	wav, err := sineWAV(48000, 1, 250, 440, 0.4)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_stem_fail", "failing take", wav); err != nil || status != 201 {
		return fmt.Errorf("failure-path source setup failed: status=%d err=%v", status, err)
	}
	_, status, err := harness.putTimeline(ctx, "timeline_stem_fail", service.TimelineWriteRequest{
		Name:           "doomed render",
		SampleRate:     48000,
		OutputChannels: 1,
		TargetPeakDBFS: -1,
		DeliveryGroups: []domain.DeliveryGroup{{ID: "vocals"}},
		Spans: []domain.TimelineSpan{
			{SourceID: "source_stem_fail", SourceStartFrame: 0, SourceEndFrame: 12000, GroupID: "vocals"},
		},
	})
	if err != nil || status != 201 {
		return fmt.Errorf("failure-path timeline setup failed: status=%d err=%v", status, err)
	}
	status, body, err := harness.requestJSON(ctx, "PUT", "/v1/renders/render_stem_fail", service.RenderWriteRequest{TimelineID: "timeline_stem_fail"})
	if err != nil {
		return err
	}
	if status != 500 {
		return fmt.Errorf("failing commit returned status %d, want 500", status)
	}
	var envelope failureEnvelope
	if err := decodeBody(body, &envelope); err != nil {
		return err
	}
	if envelope.Error.Code != "commit_simulated" || envelope.Job == nil || envelope.Job.State != domain.RenderFailed {
		return fmt.Errorf("failing commit did not produce a terminal failed job: %+v", envelope)
	}
	job, status, err := harness.getRender(ctx, "render_stem_fail")
	if err != nil {
		return err
	}
	if status != 200 || job.State != domain.RenderFailed || job.Error == nil || job.Error.Code != "commit_simulated" {
		return fmt.Errorf("failed job does not carry the stable error: %+v", job)
	}
	if job.Output != nil || len(job.Stems) != 0 {
		return fmt.Errorf("failed job exposes output metadata: %+v", job)
	}
	if _, status, _ := harness.getOutput(ctx, job.ID); status != 409 {
		return fmt.Errorf("failed render exposed its master: status=%d", status)
	}
	if _, status, _ := harness.getReceipt(ctx, job.ID); status != 409 {
		return fmt.Errorf("failed render exposed its receipt: status=%d", status)
	}
	if _, status, _ := harness.getStem(ctx, job.ID, "vocals"); status != 409 {
		return fmt.Errorf("failed render exposed a stem: status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "renders", "render_stem_fail")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed commit left a published render folder: %v", err)
	}
	return nil
}

// checkStemCommitAtomicity writes a delivery whose second stem cannot be
// staged and verifies the real repository publishes nothing at all.
func checkStemCommitAtomicity() error {
	tempDir, err := os.MkdirTemp("", "voice-timeline-check-atomic-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()
	repo, err := storage.NewRepository(tempDir)
	if err != nil {
		return err
	}
	wav, err := sineWAV(48000, 1, 100, 440, 0.3)
	if err != nil {
		return err
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: 2,
		RenderID:      "render_stem_atomic",
		TimelineID:    "timeline_stem_atomic",
	}
	stems := []domain.StemDelivery{
		{GroupID: "vocals", WAV: wav},
		{GroupID: "bad/name", WAV: wav},
	}
	if err := repo.CommitRenderArtifacts("render_stem_atomic", wav, stems, receipt); err == nil {
		return fmt.Errorf("commit with an unwritable stem succeeded")
	}
	if _, err := os.Stat(filepath.Join(tempDir, "renders", "render_stem_atomic")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("partial delivery was published: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(tempDir, "tmp"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "render-") {
			return fmt.Errorf("staging leftovers remain after failed commit: %s", entry.Name())
		}
	}
	if _, err := repo.GetRenderStem("render_stem_atomic", "vocals"); err == nil {
		return fmt.Errorf("stem of an aborted delivery is readable")
	}
	return nil
}

// windowPeakRatio compares the peak amplitude of two frame windows of a clip.
func windowPeakRatio(clip *audiocodec.Clip, startA, endA, startB, endB int64) float64 {
	reference := windowPeak(clip, startB, endB)
	if reference <= 0 {
		return 0
	}
	return windowPeak(clip, startA, endA) / reference
}

func windowPeak(clip *audiocodec.Clip, startFrame, endFrame int64) float64 {
	channels := clip.Channels
	frames := clip.FrameCount()
	if endFrame > frames {
		endFrame = frames
	}
	peak := 0.0
	for frame := startFrame; frame < endFrame; frame++ {
		for channel := 0; channel < channels; channel++ {
			value := math.Abs(float64(clip.Samples[int(frame)*channels+channel]))
			if value > peak {
				peak = value
			}
		}
	}
	return peak
}
