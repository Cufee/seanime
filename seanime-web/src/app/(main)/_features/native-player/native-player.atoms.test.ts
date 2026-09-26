import type { NativePlayer_PlaybackInfo } from "@/api/generated/types"
import { createStore } from "jotai"
import { describe, expect, it } from "vitest"
import { nativePlayer_openAtom, nativePlayer_stateAtom, nativePlayer_watchAtom } from "./native-player.atoms"
import { vc_activePlayerId } from "../video-core/video-core-atoms"

const playback = { id: "torrent-one", streamType: "torrent", deliveryFormat: "hls" } as NativePlayer_PlaybackInfo

describe("native player startup", () => {
    it("takes over from online playback when a torrent begins opening", () => {
        const store = createStore()
        store.set(vc_activePlayerId, "onlinestream")
        store.set(nativePlayer_openAtom, "Selecting torrent...")
        expect(store.get(vc_activePlayerId)).toBe("native-player")
        expect(store.get(nativePlayer_stateAtom).active).toBe(true)
    })

    it("opens playback when the loading event was missed", () => {
        const store = createStore()
        store.set(vc_activePlayerId, "onlinestream")
        expect(store.get(nativePlayer_stateAtom).active).toBe(false)
        store.set(nativePlayer_watchAtom, playback)
        expect(store.get(vc_activePlayerId)).toBe("native-player")
        expect(store.get(nativePlayer_stateAtom)).toEqual({
            active: true, playbackInfo: playback, loadingState: null, playbackError: null,
        })
    })

    it("reopens after stopping or failing a previous stream", () => {
        const store = createStore()
        store.set(nativePlayer_stateAtom, {
            active: false, playbackInfo: null, loadingState: "Ending stream...", playbackError: "Previous failure",
        })
        store.set(nativePlayer_watchAtom, playback)
        expect(store.get(nativePlayer_stateAtom).active).toBe(true)
        expect(store.get(nativePlayer_stateAtom).loadingState).toBeNull()
        expect(store.get(nativePlayer_stateAtom).playbackError).toBeNull()
    })
})
