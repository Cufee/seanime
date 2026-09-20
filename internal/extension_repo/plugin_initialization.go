package extension_repo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const pluginInitializationTimeout = 30 * time.Second

// startPluginInitializationDeadline bounds JavaScript execution during loading,
// including a UI registration invoked by the loader. The deadline only signals
// the runtimes; normal constructor cleanup retains ownership of plugin teardown.
// finish must be called on every return path and checked before publishing the
// plugin. It waits for any timeout callback so a successful load cannot receive
// a delayed initialization interrupt.
func startPluginInitializationDeadline(loader, uiVM *goja.Runtime, timeout time.Duration) (finish func() error) {
	expired := make(chan struct{})
	deadlineErr := fmt.Errorf("plugin initialization exceeded %s: %w", timeout, context.DeadlineExceeded)
	timer := time.AfterFunc(timeout, func() {
		loader.Interrupt(deadlineErr)
		uiVM.Interrupt(deadlineErr)
		close(expired)
	})
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			if !timer.Stop() {
				<-expired
				err = deadlineErr
			}
		})
		return err
	}
}
