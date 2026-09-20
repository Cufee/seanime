export function canAutoSkipChapter(chapter: { start: number; end: number }, lastUserSeekTime: number | null) {
    // A deliberate seek into a chapter means the viewer wants to watch it.
    // Subsequent seeks outside it restore normal automatic skipping.
    return lastUserSeekTime === null || lastUserSeekTime < chapter.start || lastUserSeekTime >= chapter.end
}

export function getMediaSessionSeekTime(currentTime: number, details: MediaSessionActionDetails) {
    if (details.seekTime !== undefined) return details.seekTime
    const offset = details.seekOffset ?? 10
    return currentTime + (details.action === "seekbackward" ? -offset : offset)
}
