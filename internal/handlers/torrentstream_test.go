package handlers

import (
	"net/http"
	"net/http/httptest"
	"seanime/internal/core"
	"seanime/internal/torrentstream"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestTorrentstreamStartPreservesBrowserPlayback(t *testing.T) {
	// A browser request with the external-player target must be rejected before
	// torrent selection. Dropping the browser flag would instead launch that player.
	h := &Handler{App: &core.App{TorrentstreamRepository: &torrentstream.Repository{}}}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrentstream/start", strings.NewReader(`{"browserPlayback":true,"playbackType":"default","clientId":"browser"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.HandleTorrentstreamStartStream(e.NewContext(req, rec)))
	require.Contains(t, rec.Body.String(), "browser playback requires the native player and a client ID")
}
