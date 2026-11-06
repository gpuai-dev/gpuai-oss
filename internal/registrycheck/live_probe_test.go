package registrycheck

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/imageref"
)

func TestLiveRegistries(t *testing.T) {
	if os.Getenv("REGCHECK_LIVE") == "" {
		t.Skip("set REGCHECK_LIVE=1")
	}
	c := New()
	for _, s := range []string{"ubuntu:22.04", "ghcr.io/astral-sh/uv:latest", "nvcr.io/nvidia/cuda:12.4.1-runtime-ubuntu22.04"} {
		ref, err := imageref.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		res, err := c.Check(ctx, ref, nil)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		t.Logf("%s -> digest=%s entrypoint=%v cmd=%v", s, res.Digest, res.Entrypoint, res.Cmd)
	}
	// Negative cases.
	for _, tc := range []struct{ ref, want string }{
		{"gpuai-dev/definitely-not-a-real-repo:v1", "auth-or-notfound"},
		{"ubuntu:this-tag-does-not-exist-xyz", "notfound"},
	} {
		ref, _ := imageref.Parse(tc.ref)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := c.Check(ctx, ref, nil)
		cancel()
		t.Logf("negative %s -> %v", tc.ref, err)
		if err == nil {
			t.Errorf("%s: expected error", tc.ref)
		}
	}
}
