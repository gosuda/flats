package qjs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCloseReleasesNativeRuntimeAfterGuestCancellation(t *testing.T) {
	for _, cancelGuest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelGuest), func(t *testing.T) {
			sc := &switchCtx{Context: context.Background()}
			rt, err := New(Option{Context: sc, CloseOnContextDone: true})
			if err != nil {
				t.Fatal(err)
			}
			native := rt.wrt
			host := native.Module("env")
			guest := rt.module
			if cancelGuest {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				sc.set(ctx)
				_, err := safeEval(rt, `for(;;){}`)
				cancel()
				if err == nil || !guest.IsClosed() {
					t.Fatalf("guest not cancelled: %v", err)
				}
			} else if err := guest.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			var closePanic any
			func() { defer func() { closePanic = recover() }(); rt.Close() }()
			if closePanic == nil || !strings.Contains(fmt.Sprint(closePanic), "failed to free QJS runtime") {
				t.Fatalf("teardown error not preserved: %v", closePanic)
			}
			if !host.IsClosed() {
				t.Fatal("native host module remains open")
			}
			if _, err := native.NewHostModuleBuilder("afterClose").Instantiate(context.Background()); err == nil {
				t.Fatal("native runtime remains open")
			}
			if rt.wrt != nil || rt.module != nil || rt.handle != nil || rt.context != nil || rt.registry != nil || rt.malloc != nil || rt.free != nil || rt.mem != nil {
				t.Fatal("closed runtime retains handles")
			}
			rt.Close() // Must be idempotent even after the first Close panicked.
		})
	}
}
