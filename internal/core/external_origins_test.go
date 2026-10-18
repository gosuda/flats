package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestExternalOriginsSettings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if len(e.svc.ExternalConsoleOrigins()) != 0 {
		t.Fatal("default must be empty")
	}
	const origin = "https://flats.example.org"
	settings, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetExternalOrigins: origin})
	if err != nil {
		t.Fatal(err)
	}
	if settings[core.SetExternalOrigins] != origin {
		t.Fatal(settings)
	}
	origins := e.svc.ExternalConsoleOrigins()
	if len(origins) != 1 || origins[0] != origin {
		t.Fatal(origins)
	}
	origins[0] = "https://mutated.example"
	if e.svc.ExternalConsoleOrigins()[0] != origin {
		t.Fatal("accessor aliases settings")
	}
	_, err = e.svc.UpdateSettings(ctx, map[string]string{core.SetExternalOrigins: "https://bad.example/path", core.SetVersionQuota: "12"})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("invalid origin: %v", err)
	}
	settings, _ = e.svc.Settings(ctx)
	if settings[core.SetExternalOrigins] != origin || settings[core.SetVersionQuota] == "12" {
		t.Fatal("invalid batch partially applied")
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetExternalOrigins: ""}); err != nil {
		t.Fatal(err)
	}
	if len(e.svc.ExternalConsoleOrigins()) != 0 {
		t.Fatal("removal not effective")
	}
}
