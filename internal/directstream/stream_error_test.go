package directstream

import (
	"context"
	"errors"
	"seanime/internal/mkvparser"
	"seanime/internal/player"
	"seanime/internal/util/result"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/samber/mo"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedStreamErrorReleasesPlaybackOnce(t *testing.T) {
	logger := new(zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &Manager{Logger: logger, playbackCtx: ctx, playbackCtxCancelFunc: cancel}
	var terminated atomic.Int32
	stream := &TorrentStream{BaseStream: BaseStream{
		manager: manager, logger: logger, clientId: "tv", playbackCancelFunc: cancel,
		playbackInfo:          &player.PlaybackInfo{ID: "current"},
		activeSubtitleStreams: result.NewMap[string, *SubtitleStream](),
		subtitleEventCache:    result.NewMap[string, *mkvparser.SubtitleEvent](),
	}, onTerminate: func() { terminated.Add(1) }}
	manager.currentStream = mo.Some[Stream](stream)
	subtitleCtx, cancelSubtitle := context.WithCancel(ctx)
	defer cancelSubtitle()
	stream.activeSubtitleStreams.Set("reader", &SubtitleStream{logger: logger, cleanupFunc: cancelSubtitle})
	stream.subtitleEventCache.Set("event", &mkvparser.SubtitleEvent{Text: "cached"})

	// Invoke the promoted method on the real outer stream. Comparing its
	// embedded *BaseStream with the manager's *TorrentStream loses this error.
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() { stream.StreamError(errors.New("read failed during seek")) })
	}
	callers.Wait()

	require.True(t, manager.currentStream.IsAbsent())
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.ErrorIs(t, subtitleCtx.Err(), context.Canceled)
	require.Equal(t, int32(1), terminated.Load())
	_, cached := stream.subtitleEventCache.Get("event")
	require.False(t, cached)
}

func TestEmbeddedStreamErrorPreservesReplacement(t *testing.T) {
	logger := new(zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &Manager{Logger: logger, playbackCtx: ctx, playbackCtxCancelFunc: cancel}
	var terminated atomic.Int32
	previous := &TorrentStream{BaseStream: BaseStream{manager: manager, logger: logger, clientId: "tv"},
		onTerminate: func() { terminated.Add(1) }}
	replacement := &TorrentStream{BaseStream: BaseStream{manager: manager, logger: logger, clientId: "tv"},
		onTerminate: func() { terminated.Add(1) }}
	manager.currentStream = mo.Some[Stream](replacement)

	previous.StreamError(errors.New("late error from the previous playback"))

	current, ok := manager.currentStream.Get()
	require.True(t, ok)
	require.Same(t, replacement, current)
	require.NoError(t, ctx.Err())
	require.Zero(t, terminated.Load())
}
