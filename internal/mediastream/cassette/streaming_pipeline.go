package cassette

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"seanime/internal/util"
	"strings"
)

type streamingJob struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	path    string
	err     error
}

// Streaming requests are deduplicated by segment. A seek can cancel a waiting
// request without stopping another request for the same segment. Once nobody
// needs an unfinished job, its FFmpeg process and input reads are canceled.
func (p *Pipeline) getStreamingSegment(ctx context.Context, seg int32) (string, error) {
	length, _ := p.session.Keyframes.Length()
	if seg < 0 || seg >= length {
		return "", fmt.Errorf("cassette: segment %d is outside the stream", seg)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.streamJobsMu.Lock()
	if err := p.ctx.Err(); err != nil {
		p.streamJobsMu.Unlock()
		return "", err
	}
	if p.segments.IsReady(seg) {
		p.streamJobsMu.Unlock()
		return p.segmentPath(seg), nil
	}
	job, exists := p.streamJobs[seg]
	if !exists {
		jobCtx, cancel := context.WithCancel(p.ctx)
		job = &streamingJob{done: make(chan struct{}), cancel: cancel}
		p.streamJobs[seg] = job
		p.streamJobsWg.Add(1)
		go func() {
			defer p.streamJobsWg.Done()
			defer cancel()
			path, err := p.encodeStreamingSegment(jobCtx, seg)
			p.streamJobsMu.Lock()
			job.path, job.err = path, err
			if p.streamJobs[seg] == job {
				delete(p.streamJobs, seg)
			}
			close(job.done)
			p.streamJobsMu.Unlock()
		}()
	}
	job.waiters++
	p.streamJobsMu.Unlock()
	defer func() {
		p.streamJobsMu.Lock()
		job.waiters--
		if job.waiters == 0 {
			job.cancel()
			if p.streamJobs[seg] == job {
				delete(p.streamJobs, seg)
			}
		}
		p.streamJobsMu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	case <-job.done:
		return job.path, job.err
	}
}

// Each job targets the preceding segment and the requested segment; demuxers
// may also read metadata and earlier decoder preroll. The preceding segment
// warms audio encoding and is discarded. This avoids full source scans and
// speculative reads while a browser is seeking or generating thumbnails.
func (p *Pipeline) encodeStreamingSegment(ctx context.Context, seg int32) (string, error) {
	release, err := p.governor.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.session.Out, 0755); err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(p.session.Out, ".segment-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	length, _ := p.session.Keyframes.Length()
	first := max(seg-1, 0)
	start := p.session.Keyframes.Get(first)
	end := float64(p.session.Info.Duration)
	if seg+1 < length {
		end = p.session.Keyframes.Get(seg + 1)
	}
	boundaries := append(p.session.Keyframes.Slice(first+1, seg+1), end)
	codecArgs := p.buildArgs(toSegmentStr(boundaries))
	copyVideo := p.kind == VideoKind && slicesContainPair(codecArgs, "-c:v", "copy")
	seek := start
	if copyVideo && first > 0 {
		// Seek inside the preceding GOP to avoid demuxer/DTS rounding back
		// another keyframe (notably H264 with B frames).
		// Copied packets retain their original timestamps below.
		seek = (start + p.session.Keyframes.Get(first+1)) / 2
	}

	args := []string{"-nostats", "-hide_banner", "-loglevel", "error"}
	if p.kind == VideoKind && !copyVideo {
		args = append(args, p.settings.HwAccel.DecodeFlags...)
	}
	if seek > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.6f", seek))
	}
	args = append(args, "-sn", "-dn", "-i", p.session.Path,
		"-copyts", "-start_at_zero", "-to", fmt.Sprintf("%.6f", end),
		"-map_metadata", "-1", "-map_chapters", "-1")
	args = append(args, codecArgs...)
	if p.kind == VideoKind && !copyVideo {
		// Closed, independently decodable segments need predictable timestamps.
		// B-frame reordering otherwise shifts the segmenter's initial time and
		// can make it skip a forced boundary after an accurate HTTP seek.
		args = append(args, "-level:v", "4.0", "-bf", "0")
	}
	relative := make([]float64, len(boundaries))
	for i, timestamp := range boundaries {
		relative[i] = timestamp - start
	}
	delta := "0.001"
	if p.kind == VideoKind {
		delta = "0.05"
	}
	args = append(args, "-muxdelay", "0", "-f", "segment",
		"-segment_format", "mpegts", "-segment_time_delta", delta,
		// Both muxers must retain timestamps: otherwise the first segment's
		// negative decode timestamps shift its video relative to later segments.
		"-avoid_negative_ts", "disabled",
		"-segment_format_options", "mpegts_copyts=1:avoid_negative_ts=disabled",
		"-segment_times", toSegmentStr(relative),
		"-segment_start_number", fmt.Sprint(first), filepath.Join(tmpDir, "%d.ts"))
	cmd := util.NewCmdCtx(ctx, p.settings.FfmpegPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Input URLs may include server authentication tokens.
		message := strings.ReplaceAll(stderr.String(), p.session.Path, "[source]")
		return "", fmt.Errorf("cassette: %s segment %d: %w: %s", p.label, seg, err, message)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	output := filepath.Join(tmpDir, fmt.Sprintf("%d.ts", seg))
	stat, err := os.Stat(output)
	if err != nil || stat.Size() == 0 {
		return "", fmt.Errorf("cassette: encoder did not produce %s segment %d", p.label, seg)
	}
	final := fmt.Sprintf(p.outPathFmt(0), seg)
	if err := os.Rename(output, final); err != nil {
		return "", err
	}
	p.segments.MarkReady(seg, 0)
	return final, nil
}

func slicesContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
