package extension_repo

import (
	"fmt"
	"os"
	"path/filepath"
	"seanime/internal/events"
	"seanime/internal/extension"
	"seanime/internal/goja/goja_runtime"
	"seanime/internal/hook"
	"seanime/internal/hook_resolver"
	"seanime/internal/plugin"
	"seanime/internal/util"
	"seanime/internal/util/filecache"
	"seanime/internal/util/result"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const intervalPluginSource = `
$app.onGetAnime((e) => e.next());
function init() {
    $ui.register((ctx) => {
        let ticks = 0;
        const cancel = ctx.setInterval(() => {
            console.log("lifecycle-interval-tick");
            if (++ticks === 100) cancel();
        }, 10);
    });
}`

func TestPluginFailedInitializationStopsIntervalAndUnbindsHooks(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"source", intervalPluginSource + `; init(); throw new Error("source failed after registering UI");`},
		{"init", strings.Replace(intervalPluginSource, "\n}", "\nthrow new Error('init failed after registering UI');\n}", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := newPluginLifecycleTestRepository(t)
			ext := lifecycleTestPlugin(tc.source)
			_, _, err := NewGojaPlugin(&ext, ext.Language, repo.logger, repo.gojaRuntimeManager, ws, func(string) {})
			require.Error(t, err)
			require.Zero(t, repo.hookManager.OnGetAnime().Length(), "failed constructors must detach hooks")
			require.Empty(t, ws.GetClientIds(), "failed constructors must unsubscribe their UI")
			assertPluginIntervalStopped(t, ws)
		})
	}
}

func TestPluginInitializationDeadlineCleansUp(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"source", `$app.onGetAnime((e) => e.next()); while (true) {}`},
		{"init", strings.Replace(intervalPluginSource, "\n}", "\nwhile (true) {}\n}", 1)},
		{"ui", strings.Replace(intervalPluginSource, "    });", "        while (true) {}\n    });", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := newPluginLifecycleTestRepository(t)
			ext := lifecycleTestPlugin(tc.source)
			started := time.Now()
			_, _, err := newGojaPlugin(&ext, ext.Language, repo.logger, repo.gojaRuntimeManager, ws, func(string) {}, 30*time.Millisecond)
			require.Error(t, err)
			require.Less(t, time.Since(started), time.Second, "an infinite initializer must not hold the repository lifecycle lock indefinitely")
			require.Zero(t, repo.hookManager.OnGetAnime().Length())
			require.Empty(t, ws.GetClientIds())
			assertPluginIntervalStopped(t, ws)
		})
	}
}

func TestPluginUnloadStopsIntervalAndWaitsForConcurrentCleanup(t *testing.T) {
	repo, ws := newPluginLifecycleTestRepository(t)
	ext := lifecycleTestPlugin(intervalPluginSource)
	p, _, err := NewGojaPlugin(&ext, ext.Language, repo.logger, repo.gojaRuntimeManager, ws, func(string) {})
	require.NoError(t, err)
	t.Cleanup(p.ClearInterrupt)
	require.Eventually(t, func() bool { return pluginIntervalTicks(ws) > 0 }, time.Second, 5*time.Millisecond)

	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(cleanupRelease) }) }
	t.Cleanup(release)
	var cleanupCount atomic.Int32
	p.unbindHookFuncs = append(p.unbindHookFuncs, func() {
		cleanupCount.Add(1)
		close(cleanupEntered)
		<-cleanupRelease
	})
	firstDone := make(chan struct{})
	go func() { p.ClearInterrupt(); close(firstDone) }()
	waitPluginLifecycleSignal(t, cleanupEntered)
	secondDone := make(chan struct{})
	go func() { p.ClearInterrupt(); close(secondDone) }()
	select {
	case <-secondDone:
		t.Error("a concurrent unload returned before cleanup finished")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	waitPluginLifecycleSignal(t, firstDone)
	waitPluginLifecycleSignal(t, secondDone)
	require.EqualValues(t, 1, cleanupCount.Load())
	select {
	case <-p.ui.Destroyed():
	default:
		t.Fatal("normal unload must release the UI lifetime watcher")
	}
	require.Zero(t, repo.hookManager.OnGetAnime().Length())
	require.Empty(t, ws.GetClientIds())
	assertPluginIntervalStopped(t, ws)
}

func TestPluginUnloadedHookPreservesAnAlreadyDispatchedChain(t *testing.T) {
	repo, ws := newPluginLifecycleTestRepository(t)
	ext := lifecycleTestPlugin(`$app.onGetAnime((e) => { throw new Error("stale plugin hook ran"); });`)
	p, _, err := NewGojaPlugin(&ext, ext.Language, repo.logger, repo.gojaRuntimeManager, ws, func(string) {})
	require.NoError(t, err)
	t.Cleanup(p.ClearInterrupt)
	h := repo.hookManager.OnGetAnime()
	// Trigger snapshots the handlers before this first handler unloads the
	// plugin. The copied plugin handler must still advance to the default.
	h.Bind(&hook.Handler[hook_resolver.Resolver]{Priority: -1, Func: func(e hook_resolver.Resolver) error {
		p.ClearInterrupt()
		return e.Next()
	}})
	defaultRan := false
	require.NoError(t, h.Trigger(&hook_resolver.Event{}, func(e hook_resolver.Resolver) error {
		defaultRan = true
		return nil
	}))
	require.True(t, defaultRan)
}

func TestPluginUninstallWaitsForInFlightLoadAndStopsBeforeDeletingData(t *testing.T) {
	repo, ws := newPluginLifecycleTestRepository(t)
	ext := lifecycleTestPlugin(intervalPluginSource)
	writeTestExternalExtension(t, repo.extensionDir, ext)
	repo.SetPluginSettingsPinnedTrays([]string{ext.ID})
	repo.setPluginGrantedPermissions(ext.ID, "saved-permission")
	configBucket := filecache.NewPermanentBucket(getExtensionUserConfigBucketKey(ext.ID))
	require.NoError(t, repo.fileCacher.SetPerm(configBucket, ext.ID, extension.SavedUserConfig{Version: 1}))

	loadEntered := make(chan struct{})
	loadRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(loadRelease) }) }
	t.Cleanup(release)
	ws.beforeSend = func(eventType string) {
		if eventType == events.PluginLoaded {
			close(loadEntered)
			<-loadRelease
		}
	}
	loadDone := make(chan struct{})
	go func() { repo.ReloadExternalExtension(ext.ID); close(loadDone) }()
	waitPluginLifecycleSignal(t, loadEntered)

	// Intercept persistence deletion at its real API boundary to verify the
	// runtime was already unloaded and its hooks/subscriber are gone.
	previousApp := plugin.GlobalAppContext
	var dataDeleted atomic.Bool
	plugin.GlobalAppContext = &lifecycleAppContext{AppContext: previousApp, drop: func(id string) {
		assert.Equal(t, ext.ID, id)
		_, loaded := repo.gojaExtensions.Get(id)
		assert.False(t, loaded)
		assert.Zero(t, repo.hookManager.OnGetAnime().Length())
		assert.Empty(t, ws.GetClientIds())
		dataDeleted.Store(true)
	}}
	t.Cleanup(func() { plugin.GlobalAppContext = previousApp })
	uninstallDone := make(chan error, 1)
	go func() { uninstallDone <- repo.UninstallExternalExtension(ext.ID) }()
	select {
	case err := <-uninstallDone:
		t.Errorf("uninstall finished while a new runtime was still loading: %v", err)
		uninstallDone <- err
	case <-time.After(250 * time.Millisecond):
	}
	require.Zero(t, pluginIntervalTicks(ws), "interval callbacks must wait for initialization to finish")
	release()
	waitPluginLifecycleSignal(t, loadDone)
	select {
	case err := <-uninstallDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("uninstall did not complete")
	}
	require.True(t, dataDeleted.Load(), "persistent cleanup must finish before uninstall returns")
	_, loaded := repo.gojaExtensions.Get(ext.ID)
	require.False(t, loaded, "the completing load must not resurrect an uninstalled runtime")
	_, err := os.Stat(filepath.Join(repo.extensionDir, ext.ID+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, repo.GetPluginSettings().PinnedTrayPluginIds)
	require.Empty(t, repo.GetPluginSettings().PluginGrantedPermissions)
	var savedConfig extension.SavedUserConfig
	found, err := repo.fileCacher.GetPerm(configBucket, ext.ID, &savedConfig)
	require.NoError(t, err)
	require.False(t, found)
	assertPluginIntervalStopped(t, ws)
}

func waitPluginLifecycleSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for plugin lifecycle")
	}
}

func assertPluginIntervalStopped(t *testing.T, ws *lifecycleWSEventManager) {
	t.Helper()
	ticks := pluginIntervalTicks(ws)
	require.Never(t, func() bool {
		return pluginIntervalTicks(ws) != ticks
	}, 80*time.Millisecond, 5*time.Millisecond, "an unloaded plugin must not run another interval callback")
}

func pluginIntervalTicks(ws *lifecycleWSEventManager) int {
	ticks := 0
	for _, event := range ws.Events() {
		if strings.Contains(fmt.Sprint(event.Payload), "lifecycle-interval-tick") {
			ticks++
		}
	}
	return ticks
}

func lifecycleTestPlugin(source string) extension.Extension {
	ext := testExternalExtension()
	ext.ID = "lifecycle-plugin"
	ext.Type = extension.TypePlugin
	ext.Plugin = &extension.PluginManifest{Version: extension.PluginManifestVersion}
	ext.Payload = source
	return ext
}

type lifecycleAppContext struct {
	plugin.AppContext
	drop func(string)
}

func (a *lifecycleAppContext) DropPluginData(id string) { a.drop(id) }

type lifecycleWSEventManager struct {
	*events.MockWSEventManager
	beforeSend func(string)
}

func (m *lifecycleWSEventManager) SendEvent(eventType string, payload interface{}) {
	if m.beforeSend != nil {
		m.beforeSend(eventType)
	}
	m.MockWSEventManager.SendEvent(eventType, payload)
}

func (m *lifecycleWSEventManager) UnsubscribeFromClientEvents(id string) {
	subscriber, found := m.ClientEventSubscribers.Get(id)
	m.MockWSEventManager.UnsubscribeFromClientEvents(id)
	if found {
		close(subscriber.Channel)
	}
}

func newPluginLifecycleTestRepository(t *testing.T) (*Repository, *lifecycleWSEventManager) {
	t.Helper()
	logger := new(zerolog.Nop())
	hooks := hook.NewHookManager(hook.NewHookManagerOptions{Logger: logger})
	previousHooks := hook.GlobalHookManager
	hook.SetGlobalHookManager(hooks)
	t.Cleanup(func() { hook.SetGlobalHookManager(previousHooks) })
	ws := &lifecycleWSEventManager{MockWSEventManager: events.NewMockWSEventManager(logger)}
	cacher, err := filecache.NewCacher(t.TempDir())
	require.NoError(t, err)
	// Construct only the repository lifecycle dependencies. The production
	// constructor also starts a remote extension-update checker.
	repo := &Repository{
		logger:             logger,
		extensionDir:       t.TempDir(),
		wsEventManager:     ws,
		gojaRuntimeManager: goja_runtime.NewManager(logger),
		gojaExtensions:     result.NewMap[string, GojaExtension](),
		extensionBankRef:   util.NewRef(extension.NewUnifiedBank()),
		invalidExtensions:  result.NewMap[string, *extension.InvalidExtension](),
		disabledExtensions: result.NewMap[string, *extension.Extension](),
		fileCacher:         cacher,
		hookManager:        hooks,
	}
	t.Cleanup(func() {
		repo.gojaExtensions.Range(func(_ string, ext GojaExtension) bool {
			ext.ClearInterrupt()
			return true
		})
	})
	return repo, ws
}
