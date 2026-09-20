import { describe, expect, it } from "vitest"
import { getBrowserTorrentPlaybackError, resolveTorrentPlayback } from "./browser-torrent-playback"

const preferences = {
    isElectron: false,
    browserTorrentPlayback: false,
    electronPlaybackMethod: "nativePlayer",
    torrentStreamingPlayback: "default",
    externalPlayerLink: "vlc://{url}",
}

describe("torrent playback selection", () => {
    it("preserves the existing browser preference until browser playback is enabled", () => {
        expect(resolveTorrentPlayback(preferences)).toEqual({ playbackType: "default" })
        expect(resolveTorrentPlayback({ ...preferences, torrentStreamingPlayback: "externalPlayerLink" }))
            .toEqual({ playbackType: "externalPlayerLink" })
    })

    it("uses the native player and browser delivery for an opted-in torrent", () => {
        expect(resolveTorrentPlayback({ ...preferences, browserTorrentPlayback: true }))
            .toEqual({ playbackType: "nativeplayer", browserPlayback: true })
    })

    it("overrides the shared stream preference without modifying it", () => {
        const settings = { ...preferences, torrentStreamingPlayback: "externalPlayerLink", browserTorrentPlayback: true }
        expect(resolveTorrentPlayback(settings)).toEqual({ playbackType: "nativeplayer", browserPlayback: true })
        expect(settings.torrentStreamingPlayback).toBe("externalPlayerLink")
    })

    it.each(["externalPlayerLink", "playbackmanager"] as const)("respects an explicit %s action", forcePlaybackMethod => {
        expect(resolveTorrentPlayback({ ...preferences, browserTorrentPlayback: true, forcePlaybackMethod }))
            .toEqual({ playbackType: forcePlaybackMethod === "playbackmanager" ? "default" : "externalPlayerLink" })
    })

    it("preserves browser delivery for an explicit native-player continuation", () => {
        expect(resolveTorrentPlayback({ ...preferences, browserTorrentPlayback: true, forcePlaybackMethod: "nativeplayer" }))
            .toEqual({ playbackType: "nativeplayer", browserPlayback: true })
    })

    it("honors an explicit native-player action independently of the saved preference", () => {
        expect(resolveTorrentPlayback({ ...preferences, forcePlaybackMethod: "nativeplayer" }))
            .toEqual({ playbackType: "nativeplayer", browserPlayback: true })
    })

    it("keeps Electron on its existing native delivery despite a saved browser preference", () => {
        expect(resolveTorrentPlayback({ ...preferences, isElectron: true, browserTorrentPlayback: true }))
            .toEqual({ playbackType: "nativeplayer" })
        expect(resolveTorrentPlayback({ ...preferences, isElectron: true, browserTorrentPlayback: true, electronPlaybackMethod: "default" }))
            .toEqual({ playbackType: "default" })
    })

    it("does not request an external player without a link", () => {
        expect(resolveTorrentPlayback({ ...preferences, externalPlayerLink: "", forcePlaybackMethod: "externalPlayerLink" }))
            .toEqual({ playbackType: "default" })
    })
})

describe("browser HLS capability", () => {
    const unsupported = {
        hlsJsSupported: false,
        isTypeSupported: () => false,
        canPlayType: () => "",
    }

    it("accepts Firefox/Chromium MSE with both video and audio codecs", () => {
        expect(getBrowserTorrentPlaybackError({ ...unsupported, hlsJsSupported: true, isTypeSupported: () => true })).toBeNull()
    })

    it.each(["video/mp4", "audio/mp4"])("rejects MSE when %s support is missing", missingType => {
        expect(getBrowserTorrentPlaybackError({
            ...unsupported,
            hlsJsSupported: true,
            isTypeSupported: mime => !mime.startsWith(missingType),
        })).toContain("H.264/AAC")
    })

    it("accepts native HLS without MSE, including mobile browsers", () => {
        expect(getBrowserTorrentPlaybackError({ ...unsupported, canPlayType: () => "probably" })).toBeNull()
    })

    it("requires playable codecs as well as a native HLS container", () => {
        expect(getBrowserTorrentPlaybackError({
            ...unsupported,
            canPlayType: mime => mime === "application/vnd.apple.mpegurl" ? "probably" : "",
        })).toContain("H.264/AAC")
    })

    it("requires an HLS transport even when MP4 codecs are available", () => {
        expect(getBrowserTorrentPlaybackError({ ...unsupported, isTypeSupported: () => true })).toContain("H.264/AAC")
    })
})
