import { NativePlayer_PlaybackInfo } from "@/api/generated/types"
import { atom } from "jotai"
import { atomWithImmer } from "jotai-immer"
import { vc_activePlayerId } from "../video-core/video-core-atoms"

export type NativePlayerState = {
    active: boolean
    playbackInfo: NativePlayer_PlaybackInfo | null
    playbackError: string | null
    loadingState: string | null
}

export const nativePlayer_initialState: NativePlayerState = {
    active: false,
    playbackInfo: null,
    playbackError: null,
    loadingState: null,
}

export const nativePlayer_stateAtom = atomWithImmer<NativePlayerState>(nativePlayer_initialState)

export const nativePlayer_openAtom = atom(null, (_get, set, loadingState: string) => {
    set(vc_activePlayerId, "native-player")
    set(nativePlayer_stateAtom, { active: true, playbackInfo: null, loadingState, playbackError: null })
})

// A watch event is sufficient to open playback, even if the earlier loading
// event arrived before the player mounted or while the socket reconnected.
export const nativePlayer_watchAtom = atom(null, (_get, set, playbackInfo: NativePlayer_PlaybackInfo) => {
    set(vc_activePlayerId, "native-player")
    set(nativePlayer_stateAtom, {
        active: true,
        playbackInfo,
        loadingState: null,
        playbackError: null,
    })
})

// The global player keeps its websocket listener while the entry page provides
// a destination for browser torrent playback.
export const nativePlayer_inlineSlotAtom = atom<{ element: HTMLDivElement, mediaId: number } | null>(null)
