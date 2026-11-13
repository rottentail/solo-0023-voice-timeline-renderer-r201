package render

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

type normalizationResult struct {
	Before  audiocodec.Metrics
	After   audiocodec.Metrics
	GainDB  float64
	Samples []float32
}

func normalizePeak(samples []float32, targetDBFS float64) normalizationResult {
	before := audiocodec.Measure(samples)
	gain := normalizationGain(samples, targetDBFS)
	applyGainClamped(samples, gain)
	return normalizationResult{
		Before:  before,
		After:   audiocodec.Measure(samples),
		GainDB:  audiocodec.LinearToDB(gain),
		Samples: samples,
	}
}

// normalizationGain derives the peak-normalization gain for a buffer. The
// same value is applied to the master and to every stem of a grouped render
// so relative stem loudness is preserved.
func normalizationGain(samples []float32, targetDBFS float64) float64 {
	target := audiocodec.DBToLinear(targetDBFS)
	peaks := beforePeak(samples)
	gain := 1.0
	if peaks > 0 {
		gain = target / peaks
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	return gain
}

// applyGainClamped scales a buffer in place, saturating to full scale, and
// reports how many samples had to be clamped.
func applyGainClamped(samples []float32, gain float64) int64 {
	var clipped int64
	for index, sample := range samples {
		value := float64(sample) * gain
		if value > 1 {
			value = 1
			clipped++
		}
		if value < -1 {
			value = -1
			clipped++
		}
		samples[index] = float32(value)
	}
	return clipped
}

func beforePeak(samples []float32) float64 {
	peak := 0.0
	for _, sample := range samples {
		value := math.Abs(float64(sample))
		if value > peak {
			peak = value
		}
	}
	return peak
}
