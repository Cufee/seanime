package extension_repo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
)

func TestPluginInitializationDeadlineInterruptsBothRuntimes(t *testing.T) {
	loader, uiVM := goja.New(), goja.New()
	finish := startPluginInitializationDeadline(loader, uiVM, 20*time.Millisecond)
	t.Cleanup(func() {
		loader.Interrupt("test finished")
		uiVM.Interrupt("test finished")
		_ = finish()
	})
	result := make(chan error, 1)
	go func() {
		_, err := loader.RunString("while (true) {}")
		result <- err
	}()
	select {
	case err := <-result:
		var interrupted *goja.InterruptedError
		if !errors.As(err, &interrupted) {
			t.Fatalf("initializer was not interrupted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initializer did not stop at its deadline")
	}
	if err := finish(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("finish lost the deadline error: %v", err)
	}
	if _, err := uiVM.RunString("1 + 1"); err == nil {
		t.Fatal("UI runtime remained executable after initialization expired")
	}
	if err := finish(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated finish lost the deadline error: %v", err)
	}
}

func TestFinishedPluginInitializationCannotInterruptLater(t *testing.T) {
	loader, uiVM := goja.New(), goja.New()
	finish := startPluginInitializationDeadline(loader, uiVM, 100*time.Millisecond)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := finish(); err != nil {
				t.Errorf("completed initialization was marked expired: %v", err)
			}
		}()
	}
	wg.Wait()
	time.Sleep(150 * time.Millisecond)
	for _, vm := range []*goja.Runtime{loader, uiVM} {
		value, err := vm.RunString("6 * 7")
		if err != nil || value.ToInteger() != 42 {
			t.Fatalf("initialization deadline interrupted a loaded plugin: value=%v err=%v", value, err)
		}
	}
}
