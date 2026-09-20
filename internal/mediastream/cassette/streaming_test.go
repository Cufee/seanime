package cassette

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"seanime/internal/mediastream/videofile"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func streamingTestInfo() *videofile.MediaInfo {
	codec := "avc1.4d0028"
	return &videofile.MediaInfo{
		Duration: 48,
		Video:    &videofile.Video{Codec: "h264", MimeCodec: &codec, PixFmt: "yuv420p", Width: 640, Height: 360, Bitrate: 4_000_000},
		Audios:   []videofile.Audio{{Index: 0, Codec: "flac", Channels: 1, IsDefault: true}},
	}
}

func streamingTestCassette(t *testing.T) *Cassette {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is required for streaming integration tests")
	}
	logger := zerolog.Nop()
	c, err := New(&NewCassetteOptions{Logger: &logger, HwAccelKind: "disabled", Preset: "ultrafast", FfmpegPath: ffmpeg, TempOutDir: t.TempDir(), MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Destroy)
	return c
}

func TestStreamingSessionTimelineAndValidation(t *testing.T) {
	c := streamingTestCassette(t)
	info := streamingTestInfo()
	s, err := c.NewStreamingSession(context.Background(), "http://invalid.test/stream?id=source", "fixed", info, nil)
	if err != nil {
		t.Fatal(err)
	}
	playlist, err := s.GetVideoIndex(Original, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(playlist, "segment-11.ts?token=test-token") || !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("expected immediately complete VOD timeline: %s", playlist)
	}
	if s.Ladder[0].OriginalCanTransmux || info.Audios[0].Channels != 1 || s.Info.Audios[0].Channels != 2 {
		t.Fatal("fixed grid must force video conversion and advertise stereo without mutating source metadata")
	}
	for _, index := range [][]float64{{1, 4}, {0, 4, 4}, {0, 50}, {0, math.NaN()}} {
		if _, err := c.NewStreamingSession(context.Background(), "input", fmt.Sprint(index), info, index); err == nil {
			t.Fatalf("accepted invalid keyframes %v", index)
		}
	}
	for _, segment := range []int32{-1, 12, 999999} {
		if _, err := s.GetVideoSegment(context.Background(), Original, segment); err == nil {
			t.Fatalf("accepted invalid segment %d", segment)
		}
	}
	if _, err := s.GetAudioIndex(99, ""); err == nil {
		t.Fatal("accepted unavailable audio track")
	}
	if _, err := s.GetVideoIndex(P8k, ""); err == nil {
		t.Fatal("accepted unavailable video quality")
	}
	indexed, err := c.NewStreamingSession(context.Background(), "input", "indexed", info, []float64{0, 9, 18, 27, 36, 45})
	if err != nil {
		t.Fatal(err)
	}
	playlist, _ = indexed.GetVideoIndex(Original, "")
	if !strings.Contains(playlist, "#EXT-X-TARGETDURATION:9") || !indexed.Ladder[0].OriginalCanTransmux {
		t.Fatalf("expected source-aligned remux index: %s", playlist)
	}
	for _, index := range [][]float64{{0}, {0, 2}, {0, 9, 20}} {
		sparse, err := c.NewStreamingSession(context.Background(), "input", "sparse"+fmt.Sprint(index), info, index)
		if err != nil {
			t.Fatal(err)
		}
		length, _ := sparse.Keyframes.Length()
		if sparse.Ladder[0].OriginalCanTransmux || length != 12 {
			t.Fatalf("sparse cues %v must fall back to bounded four-second segments", index)
		}
	}
	info.Video.Width, info.Video.Height = 3840, 2160
	large, err := c.NewStreamingSession(context.Background(), "input", "large", info, nil)
	if err != nil {
		t.Fatal(err)
	}
	if large.Ladder[0].Width != 1920 || large.Ladder[0].Height != 1080 || large.Ladder[0].OriginalCanTransmux {
		t.Fatal("browser conversion must stay inside the advertised H264 level")
	}
}

// These fixtures are generated locally and served through a throttled HTTP
// Range endpoint. No external media or torrent services are involved.
func TestStreamingSessionHTTPSeeking(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("FFprobe is required for streaming integration tests")
	}
	for _, fixture := range []struct {
		name, codec string
		offset      float64
	}{
		{name: "h264", codec: "h264"},
		{name: "hevc", codec: "hevc"},
		{name: "nonzero_start", codec: "h264", offset: 6},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c := streamingTestCassette(t)
			input := filepath.Join(t.TempDir(), "source.mkv")
			args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=48", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=48"}
			if fixture.codec == "h264" {
				args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "main", "-level:v", "4.0", "-bf", "2", "-g", "96", "-b:v", "4M", "-threads", "2")
			} else {
				args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-g", "48", "-b:v", "4M", "-x265-params", "pools=2:frame-threads=2:log-level=error")
			}
			args = append(args, "-c:a", "flac", "-output_ts_offset", fmt.Sprint(fixture.offset), "-y", input)
			runStreamingCommand(t, "ffmpeg", args...)
			stat, err := os.Stat(input)
			if err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int64
			var furthestRange atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if prefix, _, ok := strings.Cut(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-"); ok {
					start, _ := strconv.ParseInt(prefix, 10, 64)
					for old := furthestRange.Load(); start > old && !furthestRange.CompareAndSwap(old, start); old = furthestRange.Load() {
					}
				}
				f, err := os.Open(input)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				defer f.Close()
				http.ServeContent(&throttledStreamingWriter{ResponseWriter: w, ctx: r.Context(), sent: &sent}, r, "source.mkv", stat.ModTime(), f)
			}))
			defer server.Close()
			info := streamingTestInfo()
			var keyframes []float64
			if fixture.codec == "h264" && fixture.offset == 0 {
				for timestamp := 0.0; timestamp < 48; timestamp += 4 {
					keyframes = append(keyframes, timestamp)
				}
			} else if fixture.codec == "hevc" {
				info.Video.Codec, info.Video.PixFmt = "hevc", "yuv420p10le"
				info.Video.MimeCodec = nil
			}
			if fixture.offset != 0 {
				info, err = videofile.FfprobeGetInfo("ffprobe", input, fixture.name)
				if err != nil {
					t.Fatal(err)
				}
				if math.Abs(info.StartTime-fixture.offset) > 0.001 || math.Abs(float64(info.Duration)-(48+fixture.offset)) > 0.001 {
					t.Fatalf("fixture did not preserve nonzero Matroska timestamps: start=%f duration=%f", info.StartTime, info.Duration)
				}
				// Cues still have their original timestamps. The streaming
				// session must safely fall back to normalized encode boundaries.
				keyframes = []float64{6, 10, 14}
			}
			s, err := c.NewStreamingSession(context.Background(), server.URL+"/source.mkv", fixture.name, info, keyframes)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.offset != 0 && (s.SourceStartTime() != fixture.offset || s.Info.Duration != 48 || info.Duration != 54 || s.Ladder[0].OriginalCanTransmux) {
				t.Fatalf("incorrect source normalization: offset=%f normalized duration=%f source duration=%f", s.SourceStartTime(), s.Info.Duration, info.Duration)
			}
			for position, seg := range []int32{7, 8, 2, 0, 11} {
				video, err := s.GetVideoSegment(context.Background(), Original, seg)
				if err != nil {
					t.Fatal(err)
				}
				if position == 0 {
					if sent.Load() >= stat.Size()*3/4 {
						t.Fatalf("first far-ahead segment consumed %d of %d source bytes", sent.Load(), stat.Size())
					}
					if furthestRange.Load() < stat.Size()/3 {
						t.Fatal("far seek did not request a later source range")
					}
					assertStreamingFrame(t, video, input, float64(seg)*4, fixture.codec == "h264" && fixture.offset == 0)
				}
				audio, err := s.GetAudioSegment(context.Background(), 0, seg)
				if err != nil {
					t.Fatal(err)
				}
				videoStart, videoEnd := assertStreamingSegment(t, video, "h264", float64(seg)*4)
				audioStart, audioEnd := assertStreamingSegment(t, audio, "aac", float64(seg)*4)
				if math.Abs(videoStart-audioStart) > 0.08 || math.Abs(videoEnd-audioEnd) > 0.08 {
					t.Fatalf("audio/video are misaligned: video %.3f..%.3f, audio %.3f..%.3f", videoStart, videoEnd, audioStart, audioEnd)
				}
			}
		})
	}
}

func assertStreamingFrame(t *testing.T, segment, source string, target float64, lossless bool) {
	t.Helper()
	actual := runStreamingCommand(t, "ffmpeg", "-v", "error", "-i", segment, "-map", "0:v:0", "-frames:v", "1", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	expected := runStreamingCommand(t, "ffmpeg", "-v", "error", "-ss", fmt.Sprint(target), "-i", source, "-map", "0:v:0", "-frames:v", "1", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	if len(actual) == 0 || len(actual) != len(expected) {
		t.Fatalf("decoded source/segment frame sizes differ: %d versus %d", len(actual), len(expected))
	}
	var squaredError float64
	for i := range actual {
		difference := float64(actual[i]) - float64(expected[i])
		squaredError += difference * difference
	}
	if lossless && squaredError != 0 {
		t.Fatal("remuxed first frame differs from the requested source frame")
	}
	psnr := 10 * math.Log10(255*255/(squaredError/float64(len(actual))))
	if !lossless && psnr < 30 {
		t.Fatalf("transcoded first frame does not match requested source time: PSNR %.2f", psnr)
	}
}

type throttledStreamingWriter struct {
	http.ResponseWriter
	ctx  context.Context
	sent *atomic.Int64
}

func (w *throttledStreamingWriter) Write(data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		next := min(len(data), 32*1024)
		timer := time.NewTimer(8 * time.Millisecond)
		select {
		case <-timer.C:
		case <-w.ctx.Done():
			timer.Stop()
			return total, w.ctx.Err()
		}
		n, err := w.ResponseWriter.Write(data[:next])
		w.sent.Add(int64(n))
		total += n
		if err != nil {
			return total, err
		}
		data = data[n:]
	}
	return total, nil
}

func runStreamingCommand(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", binary, err, output)
	}
	return output
}

func assertStreamingSegment(t *testing.T, path, codec string, target float64) (float64, float64) {
	t.Helper()
	return assertStreamingSegmentDuration(t, path, codec, target, 4)
}

func assertStreamingSegmentDuration(t *testing.T, path, codec string, target, duration float64) (float64, float64) {
	t.Helper()
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
			Channels  int    `json:"channels"`
			Level     int    `json:"level"`
		} `json:"streams"`
		Packets []struct {
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
		} `json:"packets"`
	}
	output := runStreamingCommand(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,pix_fmt,channels,level:packet=pts_time,duration_time", "-of", "json", path)
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].CodecName != codec {
		t.Fatalf("unexpected output streams: %+v", probe.Streams)
	}
	stream := probe.Streams[0]
	if codec == "h264" && (stream.PixFmt != "yuv420p" || stream.Level > 40) || codec == "aac" && stream.Channels != 2 {
		t.Fatalf("output exceeds browser profile: %+v", stream)
	}
	first, last := math.Inf(1), math.Inf(-1)
	for _, packet := range probe.Packets {
		pts, err := strconv.ParseFloat(packet.PTS, 64)
		if err != nil {
			continue
		}
		duration, _ := strconv.ParseFloat(packet.Duration, 64)
		first, last = math.Min(first, pts), math.Max(last, pts+duration)
	}
	if math.Abs(first-target) > 0.08 || math.Abs(last-(target+duration)) > 0.08 {
		t.Fatalf("segment does not cover requested source time %.3f: %.3f..%.3f (%s)", target, first, last, path)
	}
	runStreamingCommand(t, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
	return first, last
}

func TestStreamingSessionRetriesTruncatedSource(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("FFprobe is required for streaming integration tests")
	}
	c := streamingTestCassette(t)
	input := filepath.Join(t.TempDir(), "source.mkv")
	runStreamingCommand(t, "ffmpeg", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=9",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=9",
		"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "main", "-level:v", "4.0",
		"-bf", "2", "-g", "96", "-c:a", "flac", "-y", input)
	source, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Packets []struct {
			PTS string `json:"pts_time"`
			Pos string `json:"pos"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(runStreamingCommand(t, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,pos", "-of", "json", input), &probe); err != nil {
		t.Fatal(err)
	}
	var cutoff, tailCutoff int64
	for _, packet := range probe.Packets {
		pts, _ := strconv.ParseFloat(packet.PTS, 64)
		if pts >= 6 && cutoff == 0 {
			cutoff, err = strconv.ParseInt(packet.Pos, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		if pts >= 8.5 {
			tailCutoff, err = strconv.ParseInt(packet.Pos, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if cutoff == 0 || tailCutoff == 0 {
		t.Fatal("could not find the fixture's truncation point")
	}
	var truncate atomic.Bool
	var truncateAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.ReadSeeker = bytes.NewReader(source)
		if truncate.Load() {
			// Advertise the real file size, then drop the response before the
			// requested segment finishes, as an interrupted torrent read would.
			reader = &truncatedStreamingReader{Reader: bytes.NewReader(source), limit: truncateAt.Load()}
		}
		http.ServeContent(w, r, "source.mkv", time.Time{}, reader)
	}))
	defer server.Close()
	for _, mode := range []string{"transcode", "remux", "audio"} {
		t.Run(mode, func(t *testing.T) {
			info := streamingTestInfo()
			info.Duration = 9
			var keyframes []float64
			if mode == "remux" {
				keyframes = []float64{0, 4, 8}
			}
			s, err := c.NewStreamingSession(context.Background(), server.URL+"/source.mkv", mode, info, keyframes)
			if err != nil {
				t.Fatal(err)
			}
			getSegment := func(seg int32) (string, error) {
				if mode == "audio" {
					return s.GetAudioSegment(context.Background(), 0, seg)
				}
				return s.GetVideoSegment(context.Background(), Original, seg)
			}
			truncateAt.Store(cutoff)
			truncate.Store(true)
			if path, err := getSegment(1); err == nil {
				t.Fatalf("published a segment from interrupted input: %s", path)
			} else if !strings.Contains(err.Error(), "incomplete segment") {
				t.Fatalf("expected incomplete output to be rejected by its timestamps: %v", err)
			}
			pipeline := s.getVideoPipeline(Original)
			codec := "h264"
			if mode == "audio" {
				pipeline = s.getAudioPipeline(0)
				codec = "aac"
			}
			if pipeline.segments.IsReady(1) {
				t.Fatal("interrupted segment was cached as ready")
			}
			truncate.Store(false)
			path, err := getSegment(1)
			if err != nil {
				t.Fatalf("retry after source recovery failed: %v", err)
			}
			assertStreamingSegment(t, path, codec, 4)
			truncateAt.Store(tailCutoff)
			truncate.Store(true)
			if path, err := getSegment(2); err == nil {
				t.Fatalf("published an interrupted final segment: %s", path)
			} else if !strings.Contains(err.Error(), "incomplete segment") {
				t.Fatalf("expected interrupted final output to be rejected by its timestamps: %v", err)
			}
			truncate.Store(false)
			path, err = getSegment(2)
			if err != nil {
				t.Fatalf("legitimate short final segment failed: %v", err)
			}
			assertStreamingSegmentDuration(t, path, codec, 8, 1)
		})
	}
}

func TestStreamingSessionAcceptsUnequalTrackTails(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("FFprobe is required for streaming integration tests")
	}
	for _, fixture := range []struct {
		name         string
		video, audio float64
	}{
		{name: "shorter_video", video: 8.7, audio: 9},
		{name: "shorter_audio", video: 9, audio: 8.7},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c := streamingTestCassette(t)
			input := filepath.Join(t.TempDir(), "source.mkv")
			runStreamingCommand(t, "ffmpeg", "-v", "error",
				"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=640x360:rate=24:duration=%g", fixture.video),
				"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%g", fixture.audio),
				"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "main", "-level:v", "4.0",
				"-bf", "2", "-g", "96", "-c:a", "flac", "-y", input)
			info := streamingTestInfo()
			info.Duration = 9
			for _, mode := range []string{"transcode", "remux"} {
				t.Run(mode, func(t *testing.T) {
					var keyframes []float64
					if mode == "remux" {
						keyframes = []float64{0, 4, 8}
					}
					s, err := c.NewStreamingSession(context.Background(), input, mode, info, keyframes)
					if err != nil {
						t.Fatal(err)
					}
					video, err := s.GetVideoSegment(context.Background(), Original, 2)
					if err != nil {
						t.Fatal(err)
					}
					audio, err := s.GetAudioSegment(context.Background(), 0, 2)
					if err != nil {
						t.Fatal(err)
					}
					assertStreamingSegmentDuration(t, video, "h264", 8, fixture.video-8)
					assertStreamingSegmentDuration(t, audio, "aac", 8, fixture.audio-8)
				})
			}
		})
	}
}

type truncatedStreamingReader struct {
	*bytes.Reader
	limit int64
}

func (r *truncatedStreamingReader) Read(p []byte) (int, error) {
	position, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if position >= r.limit {
		return 0, io.ErrUnexpectedEOF
	}
	return r.Reader.Read(p[:min(int64(len(p)), r.limit-position)])
}

func TestStreamingSessionCancellationStopsInput(t *testing.T) {
	c := streamingTestCassette(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		select {
		case <-finished:
		default:
			close(finished)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := c.NewStreamingSession(ctx, server.URL+"/source.mkv", "cancel", streamingTestInfo(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.GetVideoSegment(context.Background(), Original, 7)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("FFmpeg did not request its input")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled playback returned a segment")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled playback left a waiting segment request")
	}
	s.Kill()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled FFmpeg did not close the HTTP source")
	}
	if active := c.GovernorStats().ActiveProcesses; active != 0 {
		t.Fatalf("%d FFmpeg processes remain after cancellation", active)
	}
	if _, err := s.GetVideoSegment(context.Background(), Original, 0); err == nil {
		t.Fatal("closed session accepted new segment work")
	}
}

func TestStreamingSegmentSharedRequestCancellation(t *testing.T) {
	c := streamingTestCassette(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	var startOnce, finishOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		finishOnce.Do(func() { close(finished) })
	}))
	defer server.Close()
	s, err := c.NewStreamingSession(context.Background(), server.URL+"/source.mkv", "shared", streamingTestInfo(), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.GetVideoSegment(firstCtx, Original, 7)
		firstResult <- err
	}()
	go func() {
		_, err := s.GetVideoSegment(secondCtx, Original, 7)
		secondResult <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("FFmpeg did not request input")
	}
	pipeline := s.getVideoPipeline(Original)
	deadline := time.After(5 * time.Second)
	for {
		pipeline.streamJobsMu.Lock()
		job := pipeline.streamJobs[7]
		bothWaiting := job != nil && job.waiters == 2
		pipeline.streamJobsMu.Unlock()
		if bothWaiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("segment requests were not deduplicated")
		case <-time.After(time.Millisecond):
		}
	}
	cancelFirst()
	select {
	case err := <-firstResult:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled first request did not return")
	}
	select {
	case <-secondResult:
		t.Fatal("canceling one waiter terminated the other")
	case <-finished:
		t.Fatal("shared encoder input closed while another request needed it")
	default:
	}
	cancelSecond()
	select {
	case err := <-secondResult:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled second request did not return")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("encoder kept reading after its last request was canceled")
	}
	s.Kill()
	if count := c.GovernorStats().TotalLaunched; count != 1 {
		t.Fatalf("duplicate requests launched %d encoders", count)
	}
}

func TestStreamingCassettePreservesFileKeyframeCache(t *testing.T) {
	c := streamingTestCassette(t)
	_, err := c.NewStreamingSession(context.Background(), "input", "stream", streamingTestInfo(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := t.Name()
	kfCache.Store(key, &KeyframeIndex{})
	defer kfCache.Delete(key)
	c.Destroy()
	if _, ok := kfCache.Load(key); !ok {
		t.Fatal("closing browser playback cleared the library keyframe cache")
	}
}

var _ io.Writer = (*throttledStreamingWriter)(nil)
