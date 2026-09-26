import { nativePlayer_inlineSlotAtom } from "./native-player.atoms"
import { useSetAtom } from "jotai"
import React from "react"

export function NativePlayerInlineSlot({ mediaId }: { mediaId: number }) {
    const setSlot = useSetAtom(nativePlayer_inlineSlotAtom)
    const [element, setElement] = React.useState<HTMLDivElement | null>(null)

    React.useLayoutEffect(() => {
        if (!element) return
        setSlot({ element, mediaId })
        return () => setSlot(current => current?.element === element ? null : current)
    }, [element, mediaId, setSlot])

    return <div ref={setElement} data-torrent-stream-video-container className="w-full aspect-video mx-auto border rounded-lg overflow-hidden" />
}
