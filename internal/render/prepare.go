package render

import (
	"context"
	"errors"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

type SourceInput struct {
	Asset domain.SourceAsset
	WAV   []byte
}

type preparedSpan struct {
	Index            int
	SourceID         string
	SourceDigest     string
	SourceStartFrame int64
	SourceEndFrame   int64
	OutputStartFrame int64
	OutputEndFrame   int64
	OutputFrames     int64
	CrossfadeFrames  int64
	GainDB           float64
	ChannelAction    string
	Clip             *audiocodec.Clip
}

func prepareSpans(
	ctx context.Context,
	plan domain.Timeline,
	sources map[string]SourceInput,
) ([]preparedSpan, int64, error) {
	decoded := make(map[string]*audiocodec.Clip, len(sources))
	prepared := make([]preparedSpan, 0, len(plan.Spans))
	cursor := int64(0)
	for index, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return nil, 0, errors.New("timeline references a missing source")
		}
		clip, ok := decoded[span.SourceID]
		if !ok {
			parsed, _, err := audiocodec.DecodeWAV(source.WAV)
			if err != nil {
				return nil, 0, err
			}
			clip = parsed
			decoded[span.SourceID] = parsed
		}
		selected, err := audiocodec.SliceFrames(ctx, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			return nil, 0, err
		}
		resampled, err := audiocodec.Resample(ctx, selected, plan.SampleRate)
		if err != nil {
			return nil, 0, err
		}
		mapped, action, err := audiocodec.MapChannels(ctx, resampled, plan.OutputChannels)
		if err != nil {
			return nil, 0, err
		}
		if mapped.Channels == 2 {
			frames := mapped.FrameCount()
			peaks := [2]float64{}
			for frame := int64(0); frame < frames; frame++ {
				for channel := 0; channel < 2; channel++ {
					value := math.Abs(float64(mapped.Samples[int(frame)*2+channel]))
					if value > peaks[channel] {
						peaks[channel] = value
					}
				}
			}
			target := math.Max(peaks[0], peaks[1])
			if target > 0 {
				gains := [2]float64{1, 1}
				for channel := 0; channel < 2; channel++ {
					if peaks[channel] > 0 {
						gains[channel] = target / peaks[channel]
					}
				}
				for frame := int64(0); frame < frames; frame++ {
					for channel := 0; channel < 2; channel++ {
						index := int(frame)*2 + channel
						value := float64(mapped.Samples[index]) * gains[channel]
						mapped.Samples[index] = float32(value)
					}
				}
			}
		}
		frames := mapped.FrameCount()
		if frames < 1 {
			return nil, 0, errors.New("span produced no output frames")
		}

		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		if start < 0 {
			return nil, 0, errors.New("crossfade produced a negative output position")
		}
		end := start + frames
		cursor = end

		prepared = append(prepared, preparedSpan{
			Index:            index,
			SourceID:         span.SourceID,
			SourceDigest:     source.Asset.Digest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: start,
			OutputEndFrame:   end,
			OutputFrames:     frames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    action,
			Clip:             mapped,
		})
	}
	return prepared, cursor, nil
}
