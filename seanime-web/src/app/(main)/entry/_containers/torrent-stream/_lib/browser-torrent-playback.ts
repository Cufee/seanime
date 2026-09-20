import type { ForcePlaybackMethod } from "@/app/(main)/entry/_lib/handle-play-media"
import Hls from "hls.js"

type TorrentPlaybackOptions = {
    isElectron: boolean
    electronPlaybackMethod: string
    torrentStreamingPlayback: string
    browserTorrentPlayback: boolean
    externalPlayerLink: string
    forcePlaybackMethod?: ForcePlaybackMethod
}

export function resolveTorrentPlayback(options: TorrentPlaybackOptions): {
    playbackType: "nativeplayer" | "externalPlayerLink" | "default"
    browserPlayback?: boolean
} {
    const { forcePlaybackMethod } = options
    if (!options.isElectron && (forcePlaybackMethod === "nativeplayer" || (options.browserTorrentPlayback && !forcePlaybackMethod))) {
        return { playbackType: "nativeplayer", browserPlayback: true }
    }
    if ((!forcePlaybackMethod && options.isElectron && options.electronPlaybackMethod === "nativePlayer") || forcePlaybackMethod === "nativeplayer") {
        return { playbackType: "nativeplayer" }
    }
    if (options.externalPlayerLink && ((!forcePlaybackMethod && options.torrentStreamingPlayback === "externalPlayerLink") || forcePlaybackMethod === "externalPlayerLink")) {
        return { playbackType: "externalPlayerLink" }
    }
    return { playbackType: "default" }
}

const avcMime = "video/mp4; codecs=\"avc1.640028\""
const aacMime = "audio/mp4; codecs=\"mp4a.40.2\""

type BrowserPlaybackCapabilities = {
    hlsJsSupported: boolean
    isTypeSupported: (mime: string) => boolean
    canPlayType: (mime: string) => string
}

// Probe only when playback is requested. Browser support also depends on the OS media decoders.
export function getBrowserTorrentPlaybackError(capabilities?: BrowserPlaybackCapabilities): string | null {
    const video = capabilities ? undefined : document.createElement("video")
    const support = capabilities ?? {
        hlsJsSupported: Hls.isSupported(),
        isTypeSupported: (mime: string) => typeof MediaSource !== "undefined" && MediaSource.isTypeSupported(mime),
        canPlayType: (mime: string) => video!.canPlayType(mime),
    }
    const canUseHlsJs = support.hlsJsSupported && support.isTypeSupported(avcMime) && support.isTypeSupported(aacMime)
    const canUseNativeHls = !!support.canPlayType("application/vnd.apple.mpegurl") && !!support.canPlayType(avcMime) && !!support.canPlayType(aacMime)
    return canUseHlsJs || canUseNativeHls
        ? null
        : "This browser cannot play H.264/AAC HLS video. Enable its system media codecs or choose another playback method in Settings."
}
