package mkvparser

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestSendSubtitleEventSkipsEventsBeforeSeekTarget(t *testing.T) {
	subtitleCh := make(chan *SubtitleEvent, 1)
	event := &SubtitleEvent{StartTime: 1000, Duration: 2000}

	require.True(t, sendSubtitleEvent(context.Background(), subtitleCh, event, 4000))
	require.Empty(t, subtitleCh)
}

func TestSendSubtitleEventKeepsEventsSpanningSeekTarget(t *testing.T) {
	subtitleCh := make(chan *SubtitleEvent, 1)
	event := &SubtitleEvent{StartTime: 1000, Duration: 4000}

	require.True(t, sendSubtitleEvent(context.Background(), subtitleCh, event, 4000))
	require.Same(t, event, <-subtitleCh)
}

func TestASSSubtitleEventPreservesRenderingFields(t *testing.T) {
	logger := new(zerolog.Nop())
	parser := &MetadataParser{logger: logger}
	events := make(chan *SubtitleEvent, 1)
	parser.processSubtitleData(2, &TrackInfo{CodecID: "S_TEXT/ASS"},
		[]byte("17,3,SignStyle,Actor,0011,0022,0033,scroll up,Sign, with commas"),
		1000, 99000, 0, logger, make(map[uint8]*SubtitleEvent), events, context.Background(), nil, 0)
	require.Len(t, events, 1)
	event := <-events
	require.Equal(t, "Sign, with commas", event.Text)
	require.Equal(t, map[string]string{
		"readOrder": "17", "layer": "3", "style": "SignStyle", "name": "Actor",
		"marginL": "0011", "marginR": "0022", "marginV": "0033", "effect": "scroll up",
	}, event.ExtraData)
}

type contextSubtitleReader struct {
	ctx     context.Context
	reading chan struct{}
}

func (r *contextSubtitleReader) SetContext(ctx context.Context) { r.ctx = ctx }
func (r *contextSubtitleReader) Read([]byte) (int, error) {
	close(r.reading)
	// An unbound reader models a torrent reader waiting indefinitely for a
	// missing piece. The test supplies a deadline even if the binding regresses.
	if r.ctx == nil {
		return 0, io.ErrNoProgress
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r *contextSubtitleReader) Seek(offset int64, _ int) (int64, error) { return offset, nil }
func (r *contextSubtitleReader) Close() error                            { return nil }

func TestSubtitleClusterLookupBindsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader := &contextSubtitleReader{reading: make(chan struct{})}
	parser := &MetadataParser{logger: new(zerolog.Nop())}
	done := make(chan struct{})
	go func() {
		parser.ExtractSubtitles(ctx, reader, 100, 0, 60)
		close(done)
	}()
	select {
	case <-reader.reading:
	case <-ctx.Done():
		t.Fatal("cluster lookup did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cluster lookup did not stop after cancellation")
	}
	require.NotNil(t, reader.ctx)
	require.ErrorIs(t, reader.ctx.Err(), context.Canceled)
}

type subtitleBytesReader struct{ *bytes.Reader }

func (r subtitleBytesReader) Close() error { return nil }

func TestSubtitleColdSeekPreservesTimestampScale(t *testing.T) {
	// A cluster-only source uses a 2 ms timestamp scale from cached metadata.
	// Its subtitle starts at tick 1000 and lasts 500 ticks (2s to 3s).
	element := func(id, payload []byte) []byte {
		require.Less(t, len(payload), 127)
		data := append([]byte{}, id...)
		data = append(data, 0x80|byte(len(payload)))
		return append(data, payload...)
	}
	block := element([]byte{0xA1}, append([]byte{0x81, 0, 0, 0}, []byte("0,0,Default,,0,0,0,,Scaled subtitle")...))
	group := element([]byte{0xA0}, append(block, 0x9B, 0x82, 0x01, 0xF4))
	cluster := element([]byte{0x1F, 0x43, 0xB6, 0x75}, append([]byte{0xE7, 0x82, 0x03, 0xE8}, group...))
	reader := subtitleBytesReader{bytes.NewReader(append(make([]byte, 32), cluster...))}
	parser := &MetadataParser{
		logger: new(zerolog.Nop()),
		extractedMetadata: &Metadata{
			TimecodeScale:  2000000,
			SubtitleTracks: []*TrackInfo{{Number: 1, CodecID: "S_TEXT/ASS"}},
		},
	}
	parser.parseOnce.Do(func() {})
	parser.metadataOnce.Do(func() {})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events, errors, _ := parser.ExtractSubtitles(ctx, reader, 32, 0, 2)
	var extracted []*SubtitleEvent
	for events != nil || errors != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			extracted = append(extracted, event)
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
	require.Len(t, extracted, 1)
	require.Equal(t, 2000.0, extracted[0].StartTime)
	require.Equal(t, 1000.0, extracted[0].Duration)
	require.Equal(t, "Scaled subtitle", extracted[0].Text)
}
