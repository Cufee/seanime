package mediastream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"seanime/internal/mediastream/cassette"
	"seanime/internal/mediastream/videofile"
	"strconv"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
)

// BrowserStream owns the transcoder for one directstream playback.
type BrowserStream struct {
	session    *cassette.Session
	transcoder *cassette.Cassette
	closeOnce  sync.Once
	ctx        context.Context
	outDir     string
}

func (r *Repository) NewBrowserStream(ctx context.Context, sourceURL, id string, keyframes []float64) (*BrowserStream, error) {
	r.reqMu.Lock()
	if !r.IsInitialized() || !r.settings.MustGet().TranscodeEnabled {
		r.reqMu.Unlock()
		return nil, errors.New("enable media transcoding in the server settings to play torrents in a browser")
	}
	settings := *r.settings.MustGet()
	outDir := r.transcodeDir
	r.reqMu.Unlock()
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || outDir == "" {
		return nil, errors.New("invalid browser playback session")
	}
	if _, err := exec.LookPath(settings.FfmpegPath); err != nil {
		return nil, fmt.Errorf("FFmpeg is required for browser playback: %w", err)
	}
	info, err := videofile.FfprobeGetInfoContext(ctx, settings.FfprobePath, sourceURL, id)
	if err != nil {
		return nil, fmt.Errorf("could not inspect torrent media: %s", strings.ReplaceAll(err.Error(), sourceURL, "[source]"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionDir := filepath.Join(outDir, "browser", id)
	tc, err := cassette.New(&cassette.NewCassetteOptions{
		Logger:                r.logger,
		HwAccelKind:           settings.TranscodeHwAccel,
		Preset:                settings.TranscodePreset,
		TempOutDir:            sessionDir,
		FfmpegPath:            settings.FfmpegPath,
		FfprobePath:           settings.FfprobePath,
		HwAccelCustomSettings: settings.TranscodeHwAccelCustomSettings,
		MaxConcurrency:        2,
	})
	if err != nil {
		return nil, err
	}
	session, err := tc.NewStreamingSession(ctx, sourceURL, id, info, keyframes)
	if err != nil {
		tc.Destroy()
		_ = os.RemoveAll(sessionDir)
		return nil, err
	}
	stream := &BrowserStream{session: session, transcoder: tc, ctx: ctx, outDir: sessionDir}
	context.AfterFunc(ctx, stream.Close)
	return stream, nil
}

func (s *BrowserStream) Close() {
	s.closeOnce.Do(func() {
		s.transcoder.Destroy()
		_ = os.RemoveAll(s.outDir)
	})
}

func (s *BrowserStream) SourceStartTime() float64 { return s.session.SourceStartTime() }

func (s *BrowserStream) Duration() float64 { return float64(s.session.Info.Duration) }

func (s *BrowserStream) Serve(c echo.Context, resource string) error {
	if s.ctx.Err() != nil {
		return echo.ErrNotFound
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	token := c.QueryParam("token")
	if resource == "master.m3u8" {
		return c.Blob(http.StatusOK, "application/vnd.apple.mpegurl", []byte(s.session.GetMaster(token)))
	}
	parts := strings.Split(resource, "/")
	var quality cassette.Quality
	var audio int32
	isAudio := len(parts) == 3 && parts[0] == "audio"
	var err error
	if isAudio {
		n, parseErr := strconv.ParseInt(parts[1], 10, 32)
		if parseErr != nil || n < 0 {
			return echo.ErrNotFound
		}
		audio = int32(n)
	} else if len(parts) == 2 {
		quality, err = cassette.QualityFromString(parts[0])
		if err != nil {
			return echo.ErrNotFound
		}
	} else {
		return echo.ErrNotFound
	}
	filename := parts[len(parts)-1]
	if filename == "index.m3u8" {
		var playlist string
		if isAudio {
			playlist, err = s.session.GetAudioIndex(audio, token)
		} else {
			playlist, err = s.session.GetVideoIndex(quality, token)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "stream rendition not found")
		}
		return c.Blob(http.StatusOK, "application/vnd.apple.mpegurl", []byte(playlist))
	}
	segment, err := cassette.ParseSegment(filename)
	if err != nil || segment < 0 || filename != fmt.Sprintf("segment-%d.ts", segment) {
		return echo.ErrNotFound
	}
	length, _ := s.session.Keyframes.Length()
	if segment >= length {
		return echo.ErrNotFound
	}
	ctx, cancel := context.WithCancel(c.Request().Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	var path string
	if isAudio {
		path, err = s.session.GetAudioSegment(ctx, audio, segment)
	} else {
		path, err = s.session.GetVideoSegment(ctx, quality, segment)
	}
	if err != nil {
		if ctx.Err() != nil {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusServiceUnavailable, "could not prepare media segment").SetInternal(err)
	}
	c.Response().Header().Set("Content-Type", "video/mp2t")
	return c.File(path)
}
