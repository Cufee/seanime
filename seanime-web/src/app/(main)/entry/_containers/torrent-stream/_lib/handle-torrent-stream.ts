import { HibikeTorrent_AnimeTorrent, HibikeTorrent_BatchEpisodeFiles } from "@/api/generated/types"
import { useTorrentstreamStartStream } from "@/api/hooks/torrentstream.hooks"
import {
    useCurrentDevicePlaybackSettings,
    useExternalPlayerLink,
} from "@/app/(main)/_atoms/playback.atoms"
import { useAutoPlaySelectedTorrent, useTorrentstreamAutoplay } from "@/app/(main)/_features/autoplay/autoplay"
import { getBatchSelectionParams } from "@/app/(main)/_features/autoplay/batches.ts"
import { usePlaylistManager } from "@/app/(main)/_features/playlists/_containers/global-playlist-manager"
import { useWebsocketMessageListener } from "@/app/(main)/_hooks/handle-websockets"
import { useServerStatus } from "@/app/(main)/_hooks/use-server-status"
import {
    __torrentstream__isLoadedAtom,
    __torrentstream__loadingStateAtom,
    TorrentStreamEvents,
} from "@/app/(main)/entry/_containers/torrent-stream/playback-play-pill"
import {
    __torrentStream_autoSelectFileAtom,
    __torrentStream_currentSessionAutoSelectAtom,
} from "@/app/(main)/entry/_containers/torrent-stream/torrent-stream-page"
import { ForcePlaybackMethod, useForcePlaybackMethod } from "@/app/(main)/entry/_lib/handle-play-media"
import { clientIdAtom } from "@/app/websocket-provider"
import { logger } from "@/lib/helpers/debug"
import { WSEvents } from "@/lib/server/ws-events"
import { __isElectronDesktop__ } from "@/types/constants"
import { useAtomValue } from "jotai"
import { useSetAtom } from "jotai/react"
import React from "react"
import { toast } from "sonner"
import { getBrowserTorrentPlaybackError, resolveTorrentPlayback } from "./browser-torrent-playback"

type ManualTorrentStreamSelectionProps = {
    torrent: HibikeTorrent_AnimeTorrent
    mediaId: number
    episodeNumber: number
    aniDBEpisode: string
    chosenFileIndex: number | undefined | null
    batchEpisodeFiles: HibikeTorrent_BatchEpisodeFiles | undefined
    preload?: boolean
}
type AutoSelectTorrentStreamProps = {
    mediaId: number
    episodeNumber: number
    aniDBEpisode: string
    preload?: boolean
}

export function useHandleStartTorrentStream() {

    const { mutate, isPending } = useTorrentstreamStartStream()

    const setLoadingState = useSetAtom(__torrentstream__loadingStateAtom)
    const setIsLoaded = useSetAtom(__torrentstream__isLoadedAtom)
    const { torrentStreamingPlayback, electronPlaybackMethod, browserTorrentPlayback } = useCurrentDevicePlaybackSettings()
    const { externalPlayerLink } = useExternalPlayerLink()
    const clientId = useAtomValue(clientIdAtom)

    const setCurrentSessionAutoSelect = useSetAtom(__torrentStream_currentSessionAutoSelectAtom)

    const { resetForcePlaybackMethod, getForcePlaybackMethod } = useForcePlaybackMethod()

    const getPlaybackOptions = (forcePlaybackMethod?: ForcePlaybackMethod) => resolveTorrentPlayback({
        isElectron: __isElectronDesktop__,
        browserTorrentPlayback,
        electronPlaybackMethod,
        torrentStreamingPlayback,
        externalPlayerLink,
        forcePlaybackMethod,
    })

    function canStartBrowserPlayback(browserPlayback?: boolean, preload?: boolean) {
        if (!browserPlayback || preload) return true
        const error = !clientId ? "Waiting for the connection to Seanime. Try playing again once connected." : getBrowserTorrentPlaybackError()
        if (!error) return true
        toast.error(error)
        setLoadingState(null)
        setIsLoaded(false)
        return false
    }

    const handleStreamSelection = (params: ManualTorrentStreamSelectionProps) => {
        const forcePlaybackMethod = getForcePlaybackMethod()
        resetForcePlaybackMethod()
        const playback = getPlaybackOptions(forcePlaybackMethod)
        if (!canStartBrowserPlayback(playback.browserPlayback, params.preload)) return
        logger("TORRENT STREAM SELECTION").info("Starting torrent stream", params, playback.playbackType)
        mutate({
            mediaId: params.mediaId,
            episodeNumber: params.episodeNumber,
            torrent: params.torrent,
            aniDBEpisode: params.aniDBEpisode,
            autoSelect: false,
            fileIndex: params.chosenFileIndex ?? undefined,
            ...playback,
            clientId: clientId || "",
            batchEpisodeFiles: params.batchEpisodeFiles,
            preload: params.preload,
        }, {
            onSuccess: () => {
                // setLoadingState(null)
            },
            onError: () => {
                setLoadingState(null)
                setIsLoaded(false)
            },
        })
    }

    const handleAutoSelectStream = (params: AutoSelectTorrentStreamProps) => {
        const forcePlaybackMethod = getForcePlaybackMethod()
        resetForcePlaybackMethod()
        const playback = getPlaybackOptions(forcePlaybackMethod)
        if (!canStartBrowserPlayback(playback.browserPlayback, params.preload)) return
        logger("TORRENT STREAM SELECTION").info("Starting torrent stream (auto select)", params, playback.playbackType)
        mutate({
            mediaId: params.mediaId,
            episodeNumber: params.episodeNumber,
            aniDBEpisode: params.aniDBEpisode,
            autoSelect: true,
            torrent: undefined,
            ...playback,
            clientId: clientId || "",
            preload: params.preload,
        }, {
            onError: () => {
                setLoadingState(null)
                setIsLoaded(false)
                React.startTransition(() => {
                    setCurrentSessionAutoSelect(false)
                })
            },
        })
    }

    return {
        isUsingNativePlayer: getPlaybackOptions().playbackType === "nativeplayer",
        handleStreamSelection,
        handleAutoSelectStream,
        isPending,
    }
}

export function useTorrentStreamListener() {
    const serverStatus = useServerStatus()
    const { currentPlaylist, nextPlaylistEpisode } = usePlaylistManager()
    const { torrentstreamAutoplayInfo, autoplayNextTorrentstreamEpisode } = useTorrentstreamAutoplay()
    const { autoPlayTorrent } = useAutoPlaySelectedTorrent()
    const { handleStreamSelection, handleAutoSelectStream } = useHandleStartTorrentStream()
    const torrentStream_autoSelectFile = useAtomValue(__torrentStream_autoSelectFileAtom)

    const torrentStream_currentSessionAutoSelect = serverStatus?.torrentstreamSettings?.autoSelect

    useWebsocketMessageListener({
        type: WSEvents.TORRENTSTREAM_STATE,
        onMessage: ({ state }: { state: TorrentStreamEvents }) => {
            switch (state) {
                case TorrentStreamEvents.PreloadNextStream:
                    if (currentPlaylist && nextPlaylistEpisode) {
                        const episode = nextPlaylistEpisode.episode
                        if (!episode) return
                        if (torrentStream_currentSessionAutoSelect) {
                            logger("TORRENT STREAM LISTENER").info("Auto select is enabled, preparing next stream with auto select")
                            handleAutoSelectStream({
                                mediaId: episode.baseAnime?.id!,
                                episodeNumber: episode.episodeNumber!,
                                aniDBEpisode: episode.aniDBEpisode!,
                                preload: true,
                            })
                            return
                        } else if (
                            autoPlayTorrent?.torrent?.isBatch &&
                            torrentStream_autoSelectFile &&
                            autoPlayTorrent.entry.mediaId === episode.baseAnime?.id
                        ) {
                            logger("TORRENT STREAM LISTENER")
                                .info("Previous selection matches, preparing next stream by auto-selecting file for torrent stream")
                            const batchParams = getBatchSelectionParams(autoPlayTorrent.batchFiles, episode.episodeNumber!, episode.aniDBEpisode!)
                            handleStreamSelection({
                                mediaId: episode.baseAnime?.id!,
                                episodeNumber: episode.episodeNumber!,
                                aniDBEpisode: episode.aniDBEpisode!,
                                torrent: autoPlayTorrent.torrent,
                                chosenFileIndex: batchParams.fileIndex,
                                batchEpisodeFiles: batchParams.batchEpisodeFiles,
                                preload: true,
                            })
                            return
                        }
                    } else if (torrentstreamAutoplayInfo) {
                        logger("TORRENT STREAM LISTENER").info("Preparing next stream for episode", torrentstreamAutoplayInfo)
                        autoplayNextTorrentstreamEpisode(true)
                    }
                    break
            }
        },
    })
}
