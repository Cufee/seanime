package mediastream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"seanime/internal/database/models"
	"seanime/internal/util"
	"testing"
	"time"

	"github.com/samber/mo"
	"github.com/stretchr/testify/require"
)

func TestBrowserStreamCancelsBlockedProbe(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s required", binary)
		}
	}
	started := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	}))
	defer server.Close()
	repo := &Repository{
		logger: util.NewLogger(), transcodeDir: t.TempDir(),
		settings: mo.Some(&models.MediastreamSettings{TranscodeEnabled: true, FfmpegPath: "ffmpeg", FfprobePath: "ffprobe"}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := repo.NewBrowserStream(ctx, server.URL+"/stream", "cancel-probe", nil)
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not request its input")
	}
	cancel()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled playback left ffprobe blocked")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled probe left input request blocked")
	}
}
