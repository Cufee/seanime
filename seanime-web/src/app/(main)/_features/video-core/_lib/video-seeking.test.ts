import { describe, expect, it } from "vitest"
import { canAutoSkipChapter, getMediaSessionSeekTime } from "./video-seeking"

describe("deliberate seeking into skipped chapters", () => {
    const opening = { start: 0, end: 90 }
    const ending = { start: 1290, end: 1440 }

    it("lets repeated 15-second rewinds from the end stay in the ending", () => {
        let time = ending.end
        for (let i = 0; i < 3; i++) {
            time = getMediaSessionSeekTime(time, { action: "seekbackward", seekOffset: 15 })
            expect(canAutoSkipChapter(ending, time)).toBe(false)
        }
        expect(time).toBe(1395)
    })

    it("allows a deliberate scrub to the beginning of an opening", () => {
        expect(canAutoSkipChapter(opening, 0)).toBe(false)
    })

    it("keeps automatic skipping for chronological playback and recovery without a user seek", () => {
        expect(canAutoSkipChapter(opening, null)).toBe(true)
        expect(canAutoSkipChapter(ending, null)).toBe(true)
    })

    it("resumes auto-skip after the viewer seeks outside the chapter", () => {
        expect(canAutoSkipChapter(ending, 1400)).toBe(false)
        expect(canAutoSkipChapter(ending, 1200)).toBe(true)
        expect(canAutoSkipChapter(ending, 1440)).toBe(true)
    })

    it("keeps other chapters automatic after watching a selected opening", () => {
        expect(canAutoSkipChapter(ending, 45)).toBe(true)
    })
})

describe("Media Session seek direction", () => {
    it("interprets positive offsets according to the action", () => {
        expect(getMediaSessionSeekTime(1430, { action: "seekbackward", seekOffset: 15 })).toBe(1415)
        expect(getMediaSessionSeekTime(120, { action: "seekforward", seekOffset: 15 })).toBe(135)
    })

    it("uses ten seconds when the OS omits the offset", () => {
        expect(getMediaSessionSeekTime(120, { action: "seekbackward" })).toBe(110)
        expect(getMediaSessionSeekTime(120, { action: "seekforward" })).toBe(130)
    })

    it("preserves absolute seek-to-zero and an explicit zero offset", () => {
        expect(getMediaSessionSeekTime(120, { action: "seekto", seekTime: 0 })).toBe(0)
        expect(getMediaSessionSeekTime(120, { action: "seekbackward", seekOffset: 0 })).toBe(120)
    })
})
