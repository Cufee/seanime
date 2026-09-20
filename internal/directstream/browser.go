package directstream

import (
	"context"
	"errors"
	"seanime/internal/mkvparser"
	"seanime/internal/player"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"
)

// prepareBrowserPlayback runs after raw metadata has loaded: probing the HTTP
// input from inside LoadPlaybackInfo would recursively wait on its sync.Once.
// Keep the source info intact for range serving and subtitle extraction.
func (m *Manager) prepareBrowserPlayback(ctx context.Context, stream Stream, source *player.PlaybackInfo) (*player.PlaybackInfo, error) {
	if m.mediastreamRepository == nil || m.serverURL == "" {
		return nil, errors.New("browser media delivery is not initialized")
	}
	m.UpdateOpenStep(stream.ClientId(), "Preparing browser playback...")
	input := strings.ReplaceAll(source.StreamURL, "{{SERVER_URL}}", m.serverURL)
	delivery, err := m.mediastreamRepository.NewBrowserStream(ctx, input, source.ID, browserVideoKeyframes(source))
	if err != nil {
		return nil, err
	}
	m.playbackMu.Lock()
	if ctx.Err() != nil || !m.isCurrentStreamLocked(stream) {
		m.playbackMu.Unlock()
		delivery.Close()
		return nil, context.Canceled
	}
	stream.GetBaseStream().browserStream = delivery
	m.playbackMu.Unlock()

	prefix := "/api/v1/directstream/hls/" + source.ID + "/"
	info := *source
	info.StreamURL = "{{SERVER_URL}}" + prefix + "master.m3u8" + m.GetHMACTokenQueryParam(prefix, "?")
	info.PlaybackURI = info.StreamURL
	info.DeliveryFormat = "hls"
	info.DisablePreview = true
	info.MimeType = "application/vnd.apple.mpegurl"
	info.MkvMetadata = browserPlaybackMetadata(source.MkvMetadata, delivery.SourceStartTime(), delivery.Duration())
	return &info, nil
}

// Browser timestamps start at the first playable media packet. Preserve the
// source metadata for extraction and normalize only the copy sent to VideoCore.
func browserPlaybackMetadata(source *mkvparser.Metadata, offset, duration float64) *mkvparser.Metadata {
	if source == nil || offset == 0 {
		return source
	}
	metadata := *source
	metadata.Duration = duration
	metadata.Chapters = make([]*mkvparser.ChapterInfo, 0, len(source.Chapters))
	for _, chapter := range source.Chapters {
		if chapter == nil || (chapter.End > 0 && chapter.End <= offset) {
			continue
		}
		copy := *chapter
		copy.Start = max(0, chapter.Start-offset)
		if copy.End > 0 {
			copy.End = max(0, chapter.End-offset)
		}
		metadata.Chapters = append(metadata.Chapters, &copy)
	}
	return &metadata
}

// Only the selected video's cues can define copy-safe segment boundaries.
// Missing or nonzero-start indexes use Cassette's fixed transcode timeline.
func browserVideoKeyframes(info *player.PlaybackInfo) []float64 {
	metadata := info.MkvMetadata
	if metadata == nil || info.ContentLength <= 0 || len(metadata.VideoTracks) == 0 || metadata.VideoTracks[0] == nil {
		return nil
	}
	track := metadata.VideoTracks[0].Number
	var timestamps []float64
	for _, cue := range metadata.Cues {
		if cue == nil || int64(cue.Track) != track || cue.Position >= uint64(info.ContentLength) {
			continue
		}
		timestamp := float64(cue.Time) / 1e9
		if timestamp >= metadata.Duration {
			continue
		}
		timestamps = append(timestamps, timestamp)
	}
	sort.Float64s(timestamps)
	if len(timestamps) == 0 || timestamps[0] != 0 {
		return nil
	}
	unique := timestamps[:1]
	for _, timestamp := range timestamps[1:] {
		if timestamp != unique[len(unique)-1] {
			unique = append(unique, timestamp)
		}
	}
	return unique
}

// ServeEchoBrowserStream rejects URLs from stopped or replaced playbacks.
func (m *Manager) ServeEchoBrowserStream(c echo.Context) error {
	m.playbackMu.Lock()
	stream, ok := m.currentStream.Get()
	m.playbackMu.Unlock()
	if !ok {
		return echo.ErrNotFound
	}
	info, err := stream.LoadPlaybackInfo()
	if err != nil || info.ID != c.Param("id") {
		return echo.ErrNotFound
	}
	m.playbackMu.Lock()
	delivery := stream.GetBaseStream().browserStream
	current := m.isCurrentStreamLocked(stream)
	m.playbackMu.Unlock()
	if !current || delivery == nil {
		return echo.ErrNotFound
	}
	return delivery.Serve(c, c.Param("*"))
}
