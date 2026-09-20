package directstream

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"seanime/internal/database/models"
	"seanime/internal/events"
	"seanime/internal/library/anime"
	"seanime/internal/mediastream"
	"seanime/internal/mkvparser"
	"seanime/internal/nativeplayer"
	"seanime/internal/player"
	"seanime/internal/util"
	"seanime/internal/util/result"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/samber/mo"
	"github.com/stretchr/testify/require"
)

type subtitleTestVideoCore struct{}

// Exercise the real Matroska parser: filtering an already extracted event is
// insufficient when a cold seek starts after the block containing that event.
func TestSubtitleColdSeekPreservesOverlappingASS(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to generate the Matroska subtitle fixture")
	}
	dir := t.TempDir()
	subtitlePath := filepath.Join(dir, "signs.ass")
	require.NoError(t, os.WriteFile(subtitlePath, []byte(`[Script Info]
ScriptType: v4.00+
PlayResX: 320
PlayResY: 180
[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1
[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 1,0:00:01.00,0:01:40.00,Default,,0,0,0,,Long-lived sign
Dialogue: 0,0:00:02.00,0:00:03.00,Default,,0,0,0,,Early dialogue
Dialogue: 0,0:01:05.00,0:01:07.00,Default,,0,0,0,,Later dialogue
`), 0600))
	fixturePath := filepath.Join(dir, "subtitles.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "testsrc2=s=320x180:r=4:d=110", "-i", subtitlePath,
		"-map", "0:v:0", "-map", "1:0", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "0",
		"-threads", "1", "-g", "4", "-c:s", "ass", fixturePath)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	file, err := os.Open(fixturePath)
	require.NoError(t, err)
	defer file.Close()
	info, err := file.Stat()
	require.NoError(t, err)
	logger := zerolog.Nop()
	parser := mkvparser.NewMetadataParser(file, &logger)
	metadata := parser.GetMetadata(ctx)
	require.NoError(t, metadata.Error)
	require.NotEmpty(t, metadata.Cues)
	require.Len(t, metadata.VideoTracks, 1)
	require.Len(t, metadata.SubtitleTracks, 1)
	var videoCues, subtitleCues int
	for _, cue := range metadata.Cues {
		if int64(cue.Track) == metadata.VideoTracks[0].Number {
			videoCues++
		}
		if int64(cue.Track) == metadata.SubtitleTracks[0].Number {
			subtitleCues++
			require.Positive(t, cue.Duration)
		}
	}
	require.Positive(t, videoCues)
	require.Equal(t, 3, subtitleCues)

	extract := func(t *testing.T, offset int64, seekTime float64) []string {
		t.Helper()
		reader, err := os.Open(fixturePath)
		require.NoError(t, err)
		defer reader.Close()
		subtitles, errors, _ := parser.ExtractSubtitles(ctx, reader, offset, 0, seekTime)
		var texts []string
		for subtitles != nil || errors != nil {
			select {
			case event, ok := <-subtitles:
				if !ok {
					subtitles = nil
					continue
				}
				texts = append(texts, event.Text)
			case err, ok := <-errors:
				if !ok {
					errors = nil
					continue
				}
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		return texts
	}

	// Establish that the fixture really contains the overlapping event.
	baseline := extract(t, 0, 60)
	require.Contains(t, baseline, "Long-lived sign")
	require.Contains(t, baseline, "Later dialogue")
	for _, indexed := range []bool{true, false} {
		name := "indexed"
		if !indexed {
			name = "unindexed"
		}
		t.Run(name, func(t *testing.T) {
			meta := *metadata
			if !indexed {
				meta.Cues = nil
			}
			playbackInfo := &player.PlaybackInfo{ContentLength: info.Size(), MkvMetadata: &meta}
			for _, seekTime := range []float64{60, 2} {
				offset := subtitleOffsetForTime(playbackInfo, seekTime, metadata.Duration)
				if indexed {
					require.Positive(t, offset, "indexed text subtitles should use the cue position")
				} else {
					require.Zero(t, offset, "unindexed text subtitles need a scan from the beginning")
				}
				texts := extract(t, offset, seekTime)
				require.Contains(t, texts, "Long-lived sign", "seek=%v, offset=%v", seekTime, offset)
				require.Contains(t, texts, "Later dialogue")
			}
		})
	}
	t.Run("browser source clock", func(t *testing.T) {
		testBrowserSubtitleSourceClock(t, ctx, fixturePath)
	})
}

func testBrowserSubtitleSourceClock(t *testing.T, ctx context.Context, source string) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is required to probe the browser delivery timeline")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "shifted.mkv")
	output, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-i", source,
		"-map", "0", "-c", "copy", "-output_ts_offset", "10", path).CombinedOutput()
	require.NoError(t, err, "%s", output)
	reader, err := os.Open(path)
	require.NoError(t, err)
	defer reader.Close()
	stat, err := reader.Stat()
	require.NoError(t, err)
	logger := new(zerolog.Nop())
	parser := mkvparser.NewMetadataParser(reader, logger)
	metadata := parser.GetMetadata(ctx)
	require.NoError(t, metadata.Error)
	require.Equal(t, 120.0, metadata.Duration)

	repo := mediastream.NewRepository(&mediastream.NewRepositoryOptions{Logger: logger})
	settings := &models.MediastreamSettings{FfmpegPath: "ffmpeg", FfprobePath: "ffprobe", TranscodeHwAccel: "cpu", TranscodePreset: "ultrafast"}
	repo.InitializeModules(settings, dir, filepath.Join(dir, "transcode"))
	settings.TranscodeEnabled = true
	playbackCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	delivery, err := repo.NewBrowserStream(playbackCtx, path, "subtitle-clock", nil)
	require.NoError(t, err)
	defer delivery.Close()
	require.Equal(t, 10.0, delivery.SourceStartTime())

	ws := events.NewMockWSEventManager(logger)
	manager := &Manager{Logger: logger, playbackCtx: playbackCtx, currentPlaybackTarget: PlaybackTargetVideoCore,
		nativePlayer: nativeplayer.New(nativeplayer.NewNativePlayerOptions{WsEventManager: ws, Logger: logger, VideoCore: subtitleTestVideoCore{}}),
	}
	info := &player.PlaybackInfo{ID: "subtitle-clock", ContentLength: stat.Size(), MkvMetadata: metadata, MkvMetadataParser: mo.Some(parser)}
	stream := &LocalFileStream{localFile: &anime.LocalFile{Path: path}, BaseStream: BaseStream{
		manager: manager, logger: logger, clientId: "client", browserPlayback: true, browserStream: delivery,
		playbackInfo: info, subtitleEventCache: result.NewMap[string, *mkvparser.SubtitleEvent](),
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
	}}
	manager.currentStream = mo.Some[Stream](stream)
	// Repeating 60 models HLS metadata recovery after a seek: the new manager
	// needs the same sign again under a fresh generation.
	for i, seekTime := range []float64{0, 60, 60, 68, 2} {
		manager.startSubtitleStreamForTime(stream, info, seekTime, delivery.Duration())
		generation := int64(i + 1)
		require.Equal(t, generation, stream.subtitleGeneration.Load())
		require.Eventually(t, func() bool {
			active := false
			recordedSign := false
			stream.activeSubtitleStreams.Range(func(_ string, _ *SubtitleStream) bool {
				active = true
				return false
			})
			stream.subtitleEventCache.Range(func(_ string, event *mkvparser.SubtitleEvent) bool {
				recordedSign = recordedSign || event.Text == "Long-lived sign"
				return true
			})
			return recordedSign && !active
		}, 3*time.Second, 5*time.Millisecond)
		var got []*mkvparser.SubtitleEvent
		for _, sent := range ws.Events() {
			encoded, err := json.Marshal(sent.Payload)
			require.NoError(t, err)
			var message struct {
				Payload nativeplayer.SubtitleEventsPayload `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(encoded, &message))
			if message.Payload.GenerationID == generation {
				require.Equal(t, seekTime, message.Payload.SeekTime, "wire seek time must stay in browser seconds")
				got = append(got, message.Payload.Events...)
			}
		}
		var sign *mkvparser.SubtitleEvent
		for _, event := range got {
			if event.Text == "Long-lived sign" {
				sign = event
			}
			if seekTime == 68 {
				require.NotEqual(t, "Later dialogue", event.Text, "seek filtering must use source time")
			}
		}
		require.NotNil(t, sign)
		require.Equal(t, 1000.0, sign.StartTime)
		require.Equal(t, 99000.0, sign.Duration)
		stream.subtitleEventCache.Range(func(_ string, event *mkvparser.SubtitleEvent) bool {
			if event.Text == "Long-lived sign" {
				require.Equal(t, 11000.0, event.StartTime, "source cache must remain unshifted")
			}
			return true
		})
	}
}

func TestSubtitleEventsForPlaybackKeepsRawEvents(t *testing.T) {
	events := []*mkvparser.SubtitleEvent{
		{StartTime: 5000, Duration: 15000, CodecID: "S_TEXT/ASS", ExtraData: map[string]string{"layer": "1"}},
		{StartTime: 10000, Duration: 2000, CodecID: "S_HDMV/PGS"},
		{StartTime: 1000, Duration: 1000, CodecID: "S_TEXT/ASS"},
	}
	shifted := subtitleEventsForPlayback(events, 10)
	require.Equal(t, 0.0, shifted[0].StartTime, "JASSUB's event API requires a nonnegative start")
	require.Equal(t, 10000.0, shifted[0].Duration, "preserve the overlapping sign's original end")
	require.Equal(t, 0.0, shifted[1].StartTime)
	require.Zero(t, shifted[2].Duration, "an expired event must not receive a negative duration")
	require.Equal(t, 5000.0, events[0].StartTime)
	require.Equal(t, events[0].ExtraData, shifted[0].ExtraData)
	require.Equal(t, events, subtitleEventsForPlayback(events, 0), "native playback remains in source time")
}

func TestSubtitleRefreshRejectsReplacedStream(t *testing.T) {
	info := &player.PlaybackInfo{ID: "previous", MkvMetadataParser: mo.Some(&mkvparser.MetadataParser{})}
	previous := &BaseStream{playbackInfo: info}
	manager := &Manager{
		playbackCtx:   context.Background(),
		currentStream: mo.Some[Stream](&BaseStream{playbackInfo: &player.PlaybackInfo{ID: "replacement"}}),
	}
	manager.startSubtitleStreamForTime(previous, info, 60, 100)
	require.Zero(t, previous.subtitleGeneration.Load(), "an old event must not borrow the replacement playback context")
}

func TestSubtitleOffsetUsesSubtitleCueIntervals(t *testing.T) {
	cue := func(start, duration uint64, track uint8, position uint64) *mkvparser.CueInfo {
		return &mkvparser.CueInfo{Time: start * 1e9, Duration: duration * 1e9, Track: track, Position: position}
	}
	textTrack := &mkvparser.TrackInfo{Number: 2, CodecID: "S_TEXT/ASS"}
	cases := []struct {
		name   string
		tracks []*mkvparser.TrackInfo
		cues   []*mkvparser.CueInfo
		want   int64
	}{
		{
			name: "long sign overlaps target", tracks: []*mkvparser.TrackInfo{textTrack},
			cues: []*mkvparser.CueInfo{cue(1, 99, 2, 100), cue(50, 0, 1, 500), cue(65, 2, 2, 650)}, want: 100,
		},
		{
			name: "expired subtitles can be skipped", tracks: []*mkvparser.TrackInfo{textTrack},
			cues: []*mkvparser.CueInfo{cue(1, 2, 2, 100), cue(50, 0, 1, 500), cue(65, 2, 2, 650)}, want: 650,
		},
		{
			name: "unknown duration needs scan", tracks: []*mkvparser.TrackInfo{textTrack},
			cues: []*mkvparser.CueInfo{cue(1, 0, 2, 100), cue(65, 2, 2, 650)},
		},
		{
			name: "video-only cues need scan", tracks: []*mkvparser.TrackInfo{textTrack},
			cues: []*mkvparser.CueInfo{cue(50, 0, 1, 500)},
		},
		{
			name:   "unindexed second subtitle track needs scan",
			tracks: []*mkvparser.TrackInfo{textTrack, {Number: 3, CodecID: "S_TEXT/UTF8"}},
			cues:   []*mkvparser.CueInfo{cue(65, 2, 2, 650)},
		},
		{
			name: "PGS needs preceding decoder state", tracks: []*mkvparser.TrackInfo{{Number: 2, CodecID: "S_HDMV/PGS"}},
			cues: []*mkvparser.CueInfo{cue(65, 2, 2, 650)},
		},
		{
			name: "invalid cue offset needs scan", tracks: []*mkvparser.TrackInfo{textTrack},
			cues: []*mkvparser.CueInfo{cue(65, 2, 2, 10000)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &player.PlaybackInfo{ContentLength: 1000, MkvMetadata: &mkvparser.Metadata{SubtitleTracks: tc.tracks, Cues: tc.cues}}
			require.Equal(t, tc.want, subtitleOffsetForTime(info, 60, 100))
		})
	}
}

func (subtitleTestVideoCore) RecordEvent(*mkvparser.SubtitleEvent) {}
func (subtitleTestVideoCore) Reset()                               {}
func (subtitleTestVideoCore) Terminate()                           {}

func TestSubtitleOffsetForTimeUsesPlaybackProgress(t *testing.T) {
	// keeps seek-based subtitle refresh near the current playback position
	playbackInfo := &player.PlaybackInfo{ContentLength: subtitleBackoffBytes * 4}

	require.Equal(t, subtitleBackoffBytes, subtitleOffsetForTime(playbackInfo, 25, 100))
}

func TestSubtitleOffsetForTimeFallsBackToMetadataDuration(t *testing.T) {
	// falls back to mkv metadata when the player duration is not available yet
	playbackInfo := &player.PlaybackInfo{
		ContentLength: subtitleBackoffBytes * 4,
		MkvMetadata: &mkvparser.Metadata{
			Duration: 200,
		},
	}

	require.Equal(t, subtitleBackoffBytes, subtitleOffsetForTime(playbackInfo, 50, 0))
}

func TestSubtitleOffsetForTimeClampsNearEnd(t *testing.T) {
	// leaves enough room for the subtitle parser backoff near eof
	playbackInfo := &player.PlaybackInfo{ContentLength: subtitleBackoffBytes * 2}

	require.Equal(t, subtitleBackoffBytes, subtitleOffsetForTime(playbackInfo, 199, 200))
}

func TestStartSubtitleStreamPSkipsNearbyActiveStream(t *testing.T) {
	// avoids starting a duplicate stream when seek and range land in the same area
	reader := &trackingReadSeekCloser{}
	stream := &BaseStream{
		logger: util.NewLogger(),
		playbackInfo: &player.PlaybackInfo{
			MkvMetadataParser: mo.Some(&mkvparser.MetadataParser{}),
		},
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
	}
	stream.activeSubtitleStreams.Set("existing", &SubtitleStream{offset: 8 * 1024 * 1024})

	stream.StartSubtitleStreamP(stream, context.Background(), reader, 8*1024*1024+256*1024, subtitleBackoffBytes)

	require.True(t, reader.closed)

	count := 0
	stream.activeSubtitleStreams.Range(func(_ string, _ *SubtitleStream) bool {
		count++
		return true
	})
	require.Equal(t, 1, count)
}

func TestSubtitleFlushConfigForTorrentThrottlesBatches(t *testing.T) {
	// torrent subtitle extraction can outrun the UI, so its batches stay smaller
	defaultConfig := subtitleFlushConfigFor(player.PlaybackTypeDebrid, 0)
	torrentConfig := subtitleFlushConfigFor(player.PlaybackTypeTorrent, 0)
	torrentSeekConfig := subtitleFlushConfigFor(player.PlaybackTypeTorrent, 8*1024*1024)

	require.Less(t, torrentConfig.maxBatchSize, defaultConfig.maxBatchSize)
	require.Greater(t, torrentConfig.flushInterval, defaultConfig.flushInterval)
	require.Zero(t, defaultConfig.minSendInterval)
	require.NotZero(t, torrentConfig.minSendInterval)
	require.Less(t, torrentSeekConfig.flushInterval, torrentConfig.flushInterval)
	require.Less(t, torrentSeekConfig.minSendInterval, torrentConfig.minSendInterval)
}

func TestShouldSendSubtitleEventSkipsCachedEvents(t *testing.T) {
	// Multiple readers in one generation can rediscover the same event.
	stream := &BaseStream{subtitleEventCache: result.NewMap[string, *mkvparser.SubtitleEvent]()}
	event := &mkvparser.SubtitleEvent{
		TrackNumber: 1,
		Text:        "hello",
		StartTime:   10,
		Duration:    2,
		CodecID:     "S_TEXT/ASS",
		ExtraData:   map[string]string{"style": "Default"},
	}

	require.True(t, stream.shouldSendSubtitleEvent(event, 0))
	require.False(t, stream.shouldSendSubtitleEvent(event, 0))
	require.True(t, stream.shouldSendSubtitleEvent(&mkvparser.SubtitleEvent{
		TrackNumber: 1,
		Text:        "hello",
		StartTime:   12,
		Duration:    2,
		CodecID:     "S_TEXT/ASS",
		ExtraData:   map[string]string{"style": "Default"},
	}, 0))
}

func TestSubtitleSeekReplaysCancelledBatch(t *testing.T) {
	logger := new(zerolog.Nop())
	ws := events.NewMockWSEventManager(logger)
	manager := &Manager{
		nativePlayer: nativeplayer.New(nativeplayer.NewNativePlayerOptions{
			WsEventManager: ws, Logger: logger, VideoCore: subtitleTestVideoCore{},
		}),
		currentPlaybackTarget: PlaybackTargetVideoCore,
	}
	stream := &LocalFileStream{BaseStream: BaseStream{
		manager: manager, clientId: "client",
		playbackInfo:          &player.PlaybackInfo{ID: "playback"},
		subtitleEventCache:    result.NewMap[string, *mkvparser.SubtitleEvent](),
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
	}}
	event := &mkvparser.SubtitleEvent{Text: "Overlapping sign", StartTime: 1000, Duration: 99000}
	// Extraction records an event before its pending batch has reached the client.
	require.True(t, stream.shouldSendSubtitleEvent(event, 0))
	request := stream.beginSubtitleSeek(60)
	require.False(t, stream.sendSubtitleEvents(context.Background(), stream, []*mkvparser.SubtitleEvent{event}, subtitleFlushConfig{}, subtitleRequest{}))
	// A stale parser cannot suppress the same event from the new extraction.
	require.False(t, stream.shouldSendSubtitleEvent(event, 0))
	require.True(t, stream.shouldSendSubtitleEvent(event, request.generation))
	require.True(t, stream.sendSubtitleEvents(context.Background(), stream, []*mkvparser.SubtitleEvent{event}, subtitleFlushConfig{}, request))
	require.False(t, stream.shouldSendSubtitleEvent(event, request.generation))
	require.Len(t, ws.Events(), 1)
	encoded, err := json.Marshal(ws.Events()[0].Payload)
	require.NoError(t, err)
	var message struct {
		Payload nativeplayer.SubtitleEventsPayload `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(encoded, &message))
	require.Equal(t, request.generation, message.Payload.GenerationID)
	require.Equal(t, "Overlapping sign", message.Payload.Events[0].Text)
}

func TestBeginSubtitleSeekCancelsPreviousGeneration(t *testing.T) {
	stream := &BaseStream{
		logger:                util.NewLogger(),
		playbackInfo:          &player.PlaybackInfo{ID: "playback-1"},
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
	}
	stopped := false
	previous := &SubtitleStream{
		logger: util.NewLogger(),
		onStop: func() {
			stopped = true
		},
	}
	stream.activeSubtitleStreams.Set("previous", previous)

	request := stream.beginSubtitleSeek(125.5)

	require.True(t, stopped)
	require.Equal(t, "playback-1", request.playbackID)
	require.Equal(t, int64(1), request.generation)
	require.Equal(t, 125.5, request.seekTime)
	require.Equal(t, request.generation, stream.subtitleGeneration.Load())
}

func TestStartSubtitleStreamPRejectsStaleGeneration(t *testing.T) {
	reader := &trackingReadSeekCloser{}
	stream := &BaseStream{
		logger: util.NewLogger(),
		playbackInfo: &player.PlaybackInfo{
			MkvMetadataParser: mo.Some(&mkvparser.MetadataParser{}),
		},
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
	}
	stream.subtitleGeneration.Store(2)

	stream.startSubtitleStreamP(stream, context.Background(), reader, 0, subtitleBackoffBytes, subtitleRequest{generation: 1})

	require.True(t, reader.closed)
}

func TestSendSubtitleEventsRejectsStaleGeneration(t *testing.T) {
	stream := &BaseStream{}
	stream.subtitleGeneration.Store(2)

	sent := stream.sendSubtitleEvents(context.Background(), stream, []*mkvparser.SubtitleEvent{{Text: "stale"}}, subtitleFlushConfig{}, subtitleRequest{generation: 1})

	require.False(t, sent)
}

func TestSendSubtitleEventsIncludesPlaybackGeneration(t *testing.T) {
	logger := util.NewLogger()
	ws := events.NewMockWSEventManager(logger)
	nativePlayer := nativeplayer.New(nativeplayer.NewNativePlayerOptions{
		WsEventManager: ws,
		Logger:         logger,
		VideoCore:      subtitleTestVideoCore{},
	})
	manager := &Manager{
		nativePlayer:          nativePlayer,
		currentPlaybackTarget: PlaybackTargetVideoCore,
	}
	stream := &LocalFileStream{BaseStream: BaseStream{
		manager:  manager,
		clientId: "client",
	}}
	stream.subtitleGeneration.Store(3)
	event := &mkvparser.SubtitleEvent{Text: "current"}

	sent := stream.sendSubtitleEvents(context.Background(), stream, []*mkvparser.SubtitleEvent{event}, subtitleFlushConfig{}, subtitleRequest{
		playbackID: "playback-1",
		generation: 3,
		seekTime:   125.5,
	})

	require.True(t, sent)
	require.Len(t, ws.Events(), 1)
	payload, err := json.Marshal(ws.Events()[0].Payload)
	require.NoError(t, err)
	var message struct {
		Type    string                             `json:"type"`
		Payload nativeplayer.SubtitleEventsPayload `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(payload, &message))
	require.Equal(t, string(nativeplayer.ServerEventSubtitleEvent), message.Type)
	require.Equal(t, "playback-1", message.Payload.PlaybackID)
	require.Equal(t, int64(3), message.Payload.GenerationID)
	require.Equal(t, 125.5, message.Payload.SeekTime)
	require.Equal(t, "current", message.Payload.Events[0].Text)
}
