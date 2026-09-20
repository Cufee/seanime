package cassette

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"path/filepath"
	"seanime/internal/mediastream/videofile"
	"strconv"
	"strings"
)

// NewStreamingSession opens a seekable input with an already-known duration.
// path is a server-owned input URL or path, and id identifies this playback.
// Supplied keyframes must be validated video random-access points, starting at
// zero and ending before duration. With no keyframes, a four-second timeline is
// generated and video is always encoded, since arbitrary boundaries cannot be
// used for stream copying. No packet scan or filesystem probe is performed.
// Sparse indexes also use that fallback to avoid buffering a whole episode as
// one segment. Matroska timestamp offsets are removed from the presentation
// duration; SourceStartTime exposes the matching offset for subtitle delivery.
func (c *Cassette) NewStreamingSession(ctx context.Context, path, id string, info *videofile.MediaInfo, keyframes []float64) (*Session, error) {
	if ctx == nil || path == "" || id == "" || info == nil || info.Video == nil {
		return nil, fmt.Errorf("cassette: streaming session requires context, input, identity and video metadata")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	duration := float64(info.Duration)
	startTime := 0.0
	if info.Container != nil {
		for _, container := range strings.Split(*info.Container, ",") {
			if container == "matroska" || container == "webm" {
				// Matroska Duration is the presentation end timestamp. FFmpeg
				// -start_at_zero removes the source start from every packet, so
				// the HLS timeline must remove it from the duration as well.
				startTime = info.StartTime
				duration -= startTime
				break
			}
		}
	}
	if math.IsNaN(startTime) || math.IsInf(startTime, 0) {
		return nil, fmt.Errorf("cassette: invalid source start timestamp")
	}
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return nil, fmt.Errorf("cassette: streaming session requires a finite positive duration")
	}
	const maxSegments = 1 << 20
	if duration/4 > maxSegments || len(keyframes) > maxSegments {
		return nil, fmt.Errorf("cassette: streaming timeline is too large")
	}
	if info.Video.Width == 0 || info.Video.Height == 0 {
		return nil, fmt.Errorf("cassette: streaming session requires video dimensions")
	}

	if startTime != 0 {
		// Container cues still use source timestamps. Use the accurate
		// transcode path when normalizing a nonzero source start instead of
		// assuming the first video cue equals the first audio timestamp.
		keyframes = nil
	}
	indexed := len(keyframes) > 0
	if indexed {
		keyframes = append([]float64(nil), keyframes...)
		for i, timestamp := range keyframes {
			if math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp < 0 || timestamp >= duration ||
				(i == 0 && timestamp != 0) || (i > 0 && timestamp <= keyframes[i-1]) {
				return nil, fmt.Errorf("cassette: invalid video keyframe index")
			}
		}
		for i, timestamp := range keyframes {
			end := duration
			if i+1 < len(keyframes) {
				end = keyframes[i+1]
			}
			if end-timestamp > 10 || (len(keyframes) < 2 && duration > 4) {
				keyframes = nil
				indexed = false
				break
			}
		}
	}
	if !indexed {
		for timestamp := 0.0; timestamp < duration; timestamp += 4 {
			keyframes = append(keyframes, timestamp)
		}
	}

	// Keep source metadata immutable. The master advertises the stereo AAC
	// actually produced by the browser delivery pipeline.
	browserInfo := *info
	browserInfo.Duration = float32(duration)
	browserInfo.Audios = append([]videofile.Audio(nil), info.Audios...)
	for i := range browserInfo.Audios {
		browserInfo.Audios[i].Channels = 2
		browserInfo.Audios[i].Codec = "aac"
	}
	ladder := BuildQualityLadder(&browserInfo)
	compatibleVideo := browserRemuxableVideo(info.Video)
	filtered := ladder[:0]
	for i := range ladder {
		if ladder[i].Quality != Original && (ladder[i].Height > 1080 || ladder[i].Width > 1920) {
			continue
		}
		if ladder[i].Quality == Original {
			scale := math.Min(1, math.Min(1920/float64(info.Video.Width), 1080/float64(info.Video.Height)))
			ladder[i].Width = max(2, int32(float64(info.Video.Width)*scale)/2*2)
			ladder[i].Height = max(2, int32(float64(info.Video.Height)*scale)/2*2)
		}
		if !indexed || !compatibleVideo {
			ladder[i].OriginalCanTransmux = false
			ladder[i].NeedsTranscode = true
		}
		filtered = append(filtered, ladder[i])
	}
	ladder = filtered

	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	key := "stream:" + id
	if _, exists := c.sessions.Load(key); exists {
		return nil, fmt.Errorf("cassette: streaming session identity already exists")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		Path: path, Out: filepath.Join(c.settings.StreamDir, fmt.Sprintf("%x", sha256.Sum256([]byte(id)))),
		Info: &browserInfo, Ladder: ladder,
		Keyframes: &KeyframeIndex{Sha: id, Keyframes: keyframes, IsDone: true},
		videos:    make(map[Quality]*Pipeline), audios: make(map[int32]*Pipeline),
		settings: &c.settings, governor: c.governor, logger: c.logger,
		streaming: true, ctx: ctx, cancel: cancel, startTime: startTime,
	}
	c.sessions.Store(key, s)
	return s, nil
}

func browserRemuxableVideo(video *videofile.Video) bool {
	if !isTransmuxableVideo(video) || video.PixFmt != "yuv420p" || video.Width > 1920 || video.Height > 1080 {
		return false
	}
	codec := strings.TrimPrefix(*video.MimeCodec, "avc1.")
	if len(codec) != 6 {
		return false
	}
	level, err := strconv.ParseUint(codec[4:], 16, 8)
	return err == nil && level <= 40
}

func (s *Session) validateVideoQuality(q Quality) error {
	if !s.streaming {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for _, entry := range s.Ladder {
		if entry.Quality == q {
			return nil
		}
	}
	return fmt.Errorf("cassette: unavailable video quality %q", q)
}

func (s *Session) validateAudioTrack(index int32) error {
	if !s.streaming {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for _, audio := range s.Info.Audios {
		if int32(audio.Index) == index {
			return nil
		}
	}
	return fmt.Errorf("cassette: unavailable audio track %d", index)
}
