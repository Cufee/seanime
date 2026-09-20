package directstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"seanime/internal/database/models"
	"seanime/internal/mediastream"
	"seanime/internal/mkvparser"
	"seanime/internal/player"
	"seanime/internal/util"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"github.com/samber/mo"
	"github.com/stretchr/testify/require"
)

func TestBrowserVideoKeyframes(t *testing.T) {
	info := &player.PlaybackInfo{ContentLength: 1000, MkvMetadata: &mkvparser.Metadata{
		Duration: 12, VideoTracks: []*mkvparser.TrackInfo{{Number: 1}},
		Cues: []*mkvparser.CueInfo{
			{Track: 2, Time: 1e9, Position: 1},
			{Track: 1, Time: 4e9, Position: 300},
			{Track: 1, Time: 0, Position: 100},
			{Track: 1, Time: 4e9, Position: 300},
			{Track: 1, Time: 8e9, Position: 2000},
			{Track: 1, Time: 20e9, Position: 500},
		},
	}}
	require.Equal(t, []float64{0, 4}, browserVideoKeyframes(info))
	info.MkvMetadata.Cues[2].Time = 21e6
	require.Nil(t, browserVideoKeyframes(info), "a nonzero first cue requires conversion")
}

func TestBrowserPlaybackMetadataNormalizesChaptersWithoutChangingSource(t *testing.T) {
	metadata := &mkvparser.Metadata{Duration: 26, Chapters: []*mkvparser.ChapterInfo{
		{Start: 0, End: 3}, {Start: 5, End: 12}, {Start: 18},
	}}
	playback := browserPlaybackMetadata(metadata, 6, 20)
	require.Equal(t, 20.0, playback.Duration)
	require.Len(t, playback.Chapters, 2)
	require.Equal(t, 0.0, playback.Chapters[0].Start)
	require.Equal(t, 6.0, playback.Chapters[0].End)
	require.Equal(t, 12.0, playback.Chapters[1].Start)
	require.Equal(t, 26.0, metadata.Duration)
	require.Equal(t, 5.0, metadata.Chapters[1].Start)
}

type browserAttachmentTestStream struct{ testStream }

func (s *browserAttachmentTestStream) GetAttachmentByName(string) (*mkvparser.AttachmentInfo, bool) {
	return &mkvparser.AttachmentInfo{Mimetype: "font/ttf", Data: []byte("episode font")}, true
}

func TestBrowserAttachmentsRequirePlaybackIdentity(t *testing.T) {
	manager := &Manager{}
	stream := &browserAttachmentTestStream{testStream{BaseStream: BaseStream{
		browserPlayback: true, playbackInfo: &player.PlaybackInfo{ID: "episode-two"},
	}}}
	manager.currentStream = mo.Some[Stream](stream)
	e := echo.New()
	e.GET("/att/*", manager.ServeEchoAttachments)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", http.StatusNotFound},
		{"?id=episode-one", http.StatusNotFound},
		{"?id=episode-two", http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/att/font.ttf"+tc.query, nil))
		require.Equal(t, tc.status, recorder.Code)
	}
	stream.browserPlayback = false
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/att/font.ttf", nil))
	require.Equal(t, http.StatusOK, recorder.Code, "legacy clients keep existing attachment URLs")
}

// Exercise the actual URL probe and HLS handlers, including a seek to a segment
// that has not been generated. All input is synthetic and served on loopback.
func TestBrowserPlaybackDeliveryLifecycle(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is required for media integration test", binary)
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=160x90:r=24", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "9", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-c:a", "aac", file).CombinedOutput()
	require.NoError(t, err, "%s", output)

	logger := zerolog.Nop()
	repo := mediastream.NewRepository(&mediastream.NewRepositoryOptions{Logger: &logger})
	// Initializing with conversion disabled avoids an unused library transcoder.
	settings := &models.MediastreamSettings{FfmpegPath: "ffmpeg", FfprobePath: "ffprobe", TranscodeHwAccel: "cpu", TranscodePreset: "ultrafast"}
	repo.InitializeModules(settings, dir, filepath.Join(dir, "transcode"))
	settings.TranscodeEnabled = true
	manager := &Manager{Logger: &logger, mediastreamRepository: repo, playbackCtx: ctx}
	auth := util.NewHMACAuth("browser-test-password", time.Hour)
	manager.hmacTokenFunc = func(endpoint, symbol string) string {
		query, err := auth.GenerateQueryParam(endpoint, symbol)
		require.NoError(t, err)
		return query
	}
	e := echo.New()
	defaultErrorHandler := e.HTTPErrorHandler
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		t.Logf("HTTP handler: %v", err)
		defaultErrorHandler(err, c)
	}
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if _, err := auth.ValidateToken(c.QueryParam("token"), c.Request().URL.Path); err != nil {
				return echo.ErrUnauthorized
			}
			return next(c)
		}
	})
	e.GET("/api/v1/directstream/stream", echo.WrapHandler(manager.ServeEchoStream()))
	e.GET("/api/v1/directstream/hls/:id/*", manager.ServeEchoBrowserStream)
	server := httptest.NewServer(e)
	defer server.Close()
	manager.serverURL = server.URL
	raw := &player.PlaybackInfo{ID: "episode-one", PlaybackType: player.PlaybackTypeTorrent, StreamURL: "{{SERVER_URL}}/api/v1/directstream/stream?id=episode-one" + manager.GetHMACTokenQueryParam("/api/v1/directstream/stream", "&")}
	stream := &testStream{BaseStream: BaseStream{manager: manager, logger: &logger, browserPlayback: true, playbackInfo: raw}, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, file) })}
	manager.currentStream = mo.Some[Stream](stream)
	info, err := manager.prepareBrowserPlayback(ctx, stream, raw)
	require.NoError(t, err)
	defer stream.browserStream.Close()
	require.Empty(t, raw.DeliveryFormat, "raw source metadata must not be overwritten")
	require.Contains(t, raw.StreamURL, "/stream?id=")
	require.Equal(t, "hls", info.DeliveryFormat)
	require.True(t, info.DisablePreview)
	require.Equal(t, raw.PlaybackType, info.PlaybackType)
	require.Equal(t, raw.ID, info.ID)

	masterURL := strings.ReplaceAll(info.StreamURL, "{{SERVER_URL}}", server.URL)
	request := func(address string, status int) string {
		t.Helper()
		response, err := server.Client().Get(address)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, status, response.StatusCode, "%s", body)
		return string(body)
	}
	master := request(masterURL, http.StatusOK)
	require.Contains(t, master, "#EXTM3U")
	parsed, err := url.Parse(masterURL)
	require.NoError(t, err)
	token := parsed.RawQuery
	base := server.URL + "/api/v1/directstream/hls/episode-one/"
	index := request(base+"original/index.m3u8?"+token, http.StatusOK)
	require.Contains(t, index, "#EXT-X-ENDLIST")
	require.Contains(t, index, "segment-2.ts?")
	segment := request(base+"original/segment-2.ts?"+token, http.StatusOK)
	require.NotEmpty(t, segment)
	request(base+"original/segment-2junk.ts?"+token, http.StatusNotFound)
	request(base+"original/segment-3.ts?"+token, http.StatusNotFound)
	request(base+"audio/999/index.m3u8?"+token, http.StatusNotFound)
	request(base+"master.m3u8", http.StatusUnauthorized)
	request(strings.Replace(masterURL, "episode-one/", "episode-two/", 1), http.StatusUnauthorized)

	manager.playbackMu.Lock()
	manager.currentStream = mo.Some[Stream](&testStream{BaseStream: BaseStream{playbackInfo: &player.PlaybackInfo{ID: "episode-two"}}})
	manager.playbackMu.Unlock()
	request(masterURL, http.StatusNotFound)
	manager.playbackMu.Lock()
	manager.currentStream = mo.Some[Stream](stream)
	manager.playbackMu.Unlock()
	cancel()
	request(masterURL, http.StatusNotFound)
	manager.playbackMu.Lock()
	manager.currentStream = mo.None[Stream]()
	manager.playbackMu.Unlock()
	request(masterURL, http.StatusNotFound)
}
