# Browser torrent playback

Play torrent streams inside Seanime's existing player, with seeking, embedded
subtitles, fonts, and subtitle track selection.

## Enable

1. Run this fork on the server. The [rootless Docker image](docker.md) includes
   FFmpeg and FFprobe.
2. Enable media transcoding in Seanime's server settings. In Docker, use
   `/usr/bin/ffmpeg` and `/usr/bin/ffprobe`.
3. Enable **Play torrents in this browser** in the device's playback settings.
4. Start a torrent through the normal episode selection flow.

This preference applies per browser. Torrent playback uses Seanime's built-in
engine; Transmission and VLC are not required. The browser must support H.264
and AAC through HLS.js/MediaSource or native HLS. Some Linux Firefox installations
require system media codecs.

## Behavior and limits

- Browser torrents use the online streaming layout: an inline player, episode
  sidebar, list/grid selector, and theater mode. Torrent auto-selection and
  manual selection remain available above the player.
- Compatible H.264 video can be remuxed. Other sources are converted to H.264/AAC,
  with converted video limited to 1080p/30fps and audio to stereo.
- Seeking requests the needed segments directly. Missing torrent pieces can
  delay playback and subtitles, especially after a cold seek.
- Embedded ASS styling and fonts use the existing subtitle renderer. Separate
  subtitle files inside the torrent are not supported by this change.
- Thumbnail previews are disabled to avoid competing torrent reads.
- Seanime still allows one active torrent playback; simultaneous independent
  TV and browser sessions are not supported.
- Firefox and Chromium are supported by the tested delivery path. Safari/mobile,
  HDR conversion, and hardware acceleration need separate validation. iOS native
  fullscreen may omit subtitle overlays; use inline playback if this occurs.

The integration reuses NativePlayer/VideoCore, the existing subtitle event path,
and Cassette for on-demand HLS delivery. Stopping or replacing playback cancels
its encoder jobs and removes temporary output. Parser fixes, media delivery, and
player integration are kept in separate commits to simplify upstream updates.
