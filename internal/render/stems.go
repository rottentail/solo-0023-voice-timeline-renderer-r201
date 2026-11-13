package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// Delivery quantization policy. The 16-bit encoder rounds to the nearest
// integer (half away from zero) without dither; the receipt publishes these
// facts so deviations between the quantized master and the quantized stems
// are fully explained by quantization.
const (
	quantizationFormat   = "pcm_s16le"
	quantizationRounding = "round_half_away_from_zero"
	quantizationDither   = "none"
	quantizationLSB      = 1.0 / 32768.0

	// reconstructionRule states that the unquantized stem buffers sum to the
	// unquantized master production signal.
	reconstructionRule = "sum_of_unquantized_stems_equals_master"

	// deviationQuantumLSB bounds one quantization step in LSB units; the
	// positive and negative full-scale steps differ by 32768/32767.
	deviationQuantumLSB = 0.5 * 32768.0 / 32767.0

	// deviationMarginLSB covers float32 summation and decoding precision on
	// top of the pure quantization bound.
	deviationMarginLSB = 0.01
)

// stemGroup is one declared delivery group with its span positions in
// timeline order.
type stemGroup struct {
	id    string
	label string
	spans []int
}

// StemResult is one encoded stem plus its delivery summary.
type StemResult struct {
	GroupID string
	WAV     []byte
	Receipt domain.StemReceipt
}

// renderGrouped renders a timeline with declared delivery groups into a
// time-aligned master and one stem per group. The master is, by
// construction, the exact float sum of the unquantized stem buffers, and one
// shared normalization gain is applied to the master and every stem so the
// relative loudness of the groups is preserved.
//
// Working memory stays independent of the group count: sources are decoded
// and prepared once, and a single scratch buffer is reused for every group.
func renderGrouped(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	spans []preparedSpan,
	totalFrames int64,
	now time.Time,
) (Result, error) {
	channels := plan.OutputChannels
	if totalFrames > int64(^uint(0)>>1)/int64(channels) {
		return Result{}, errors.New("mix is too large for this process")
	}
	groups, err := groupSpans(plan, spans)
	if err != nil {
		return Result{}, err
	}
	sampleCount := int(totalFrames) * channels
	master := make([]float32, sampleCount)
	scratch := make([]float32, sampleCount)

	// First pass: accumulate the unscaled production signal to derive the
	// shared normalization gain from the master peak.
	for _, group := range groups {
		if err := accumulateGroup(ctx, spans, group, scratch, channels); err != nil {
			return Result{}, err
		}
		for index := range master {
			master[index] += scratch[index]
		}
	}
	before := audiocodec.Measure(master)
	gain := normalizationGain(master, plan.TargetPeakDBFS)

	// Second pass: scale every group with the shared gain and accumulate the
	// master as the exact sum of the delivered stem buffers.
	for index := range master {
		master[index] = 0
	}
	stems := make([]StemResult, 0, len(groups))
	stemReceipts := make([]domain.StemReceipt, 0, len(groups))
	warnings := make([]string, 0)
	for _, group := range groups {
		if err := accumulateGroup(ctx, spans, group, scratch, channels); err != nil {
			return Result{}, err
		}
		clipped := applyGainClamped(scratch, gain)
		for index := range master {
			master[index] += scratch[index]
		}
		encoded, err := audiocodec.EncodePCM16(plan.SampleRate, channels, scratch)
		if err != nil {
			return Result{}, err
		}
		summary, err := buildStemReceipt(group, spans, encoded)
		if err != nil {
			return Result{}, err
		}
		if clipped > 0 {
			warnings = append(warnings, fmt.Sprintf("stem %s saturated %d samples at full scale", group.id, clipped))
		}
		stems = append(stems, StemResult{GroupID: group.id, WAV: encoded, Receipt: summary})
		stemReceipts = append(stemReceipts, summary)
	}
	if clipped := applyGainClamped(master, 1); clipped > 0 {
		warnings = append(warnings, fmt.Sprintf("master saturated %d samples at full scale", clipped))
	}
	normalized := normalizationResult{
		Before:  before,
		After:   audiocodec.Measure(master),
		GainDB:  audiocodec.LinearToDB(gain),
		Samples: master,
	}
	encodedMaster, err := audiocodec.EncodePCM16(plan.SampleRate, channels, master)
	if err != nil {
		return Result{}, err
	}
	receipt, output, err := buildReceipt(renderID, plan, encodedMaster, normalized, spans, now)
	if err != nil {
		return Result{}, err
	}
	receipt.SchemaVersion = 2
	receipt.Delivery = &domain.DeliveryReceipt{
		SharedGainDB: normalized.GainDB,
		Quantization: domain.QuantizationReceipt{
			Format:   quantizationFormat,
			Rounding: quantizationRounding,
			Dither:   quantizationDither,
			LSB:      quantizationLSB,
		},
		Reconstruction: domain.ReconstructionReceipt{
			Rule:            reconstructionRule,
			MaxDeviationLSB: float64(len(groups)+1) * (deviationQuantumLSB + deviationMarginLSB),
		},
		Stems: stemReceipts,
	}
	receipt.Warnings = append(receipt.Warnings, warnings...)
	return Result{WAV: encodedMaster, Stems: stems, Receipt: receipt, Output: output}, nil
}

// groupSpans maps every prepared span to its declared delivery group,
// keeping the declaration order of the groups.
func groupSpans(plan domain.Timeline, spans []preparedSpan) ([]stemGroup, error) {
	slots := make(map[string]int, len(plan.DeliveryGroups))
	groups := make([]stemGroup, 0, len(plan.DeliveryGroups))
	for _, declared := range plan.DeliveryGroups {
		slots[declared.ID] = len(groups)
		groups = append(groups, stemGroup{id: declared.ID, label: declared.Label})
	}
	for position, span := range spans {
		slot, ok := slots[span.GroupID]
		if !ok {
			return nil, fmt.Errorf("span %d references an undeclared delivery group", span.Index)
		}
		groups[slot].spans = append(groups[slot].spans, position)
	}
	return groups, nil
}

// accumulateGroup zeroes the buffer and adds every span contribution of the
// group in timeline order.
func accumulateGroup(ctx context.Context, spans []preparedSpan, group stemGroup, buffer []float32, channels int) error {
	for index := range buffer {
		buffer[index] = 0
	}
	for _, position := range group.spans {
		if err := addContribution(ctx, spans, position, buffer, channels); err != nil {
			return err
		}
	}
	return nil
}

// addContribution mixes one span into the buffer with its gain and fade
// envelope. Equal-power fades apply only at junctions between spans of the
// same delivery group; at a cross-group junction both neighbors keep their
// full gain so one group's junction never fades another group's audio.
func addContribution(ctx context.Context, spans []preparedSpan, position int, buffer []float32, channels int) error {
	span := spans[position]
	headFade := int64(0)
	if position > 0 && spans[position-1].GroupID == span.GroupID {
		headFade = span.CrossfadeFrames
	}
	tailFade := int64(0)
	if position+1 < len(spans) && spans[position+1].GroupID == span.GroupID {
		tailFade = spans[position+1].CrossfadeFrames
	}
	gain := float32(audiocodec.DBToLinear(span.GainDB))
	frames := span.Clip.FrameCount()
	for frame := int64(0); frame < frames; frame++ {
		if frame%4096 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		weight := float64(1)
		if headFade > 0 && frame < headFade {
			progress := (float64(frame) + 0.5) / float64(headFade)
			weight *= math.Sin(progress * math.Pi / 2)
		}
		if tailFade > 0 {
			tailStart := frames - tailFade
			if frame >= tailStart {
				progress := (float64(frame-tailStart) + 0.5) / float64(tailFade)
				weight *= math.Cos(progress * math.Pi / 2)
			}
		}
		scaled := float32(weight)
		inStart := int(frame) * channels
		outStart := int(span.OutputStartFrame+frame) * channels
		for channel := 0; channel < channels; channel++ {
			buffer[outStart+channel] += span.Clip.Samples[inStart+channel] * gain * scaled
		}
	}
	return nil
}
