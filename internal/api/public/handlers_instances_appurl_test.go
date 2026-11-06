package public

import (
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
)

// HG-01 regression: instanceToPublic is the production read-path mapper for
// GET /v1/instances{,/{id}}. It must derive connection.app_url for a running
// template instance (so a caller who didn't capture the one-time deploy output
// can recover the URL), and must NOT emit it for raw instances or when AppsHost
// is unconfigured. The privacy test only marshalled a hand-built struct, so this
// gap was previously invisible to the suite.
func TestInstanceToPublic_AppURL(t *testing.T) {
	port := 10005
	tmpl := "comfyui"
	mk := func(status string, template bool) *db.Instance {
		inst := &db.Instance{InstanceID: "gpu-abcd", Status: status, FRPRemotePort: &port}
		if template {
			inst.TemplateID = &tmpl
		}
		return inst
	}

	t.Run("running template instance carries app_url", func(t *testing.T) {
		out := instanceToPublic(mk("running", true), "frp.example.com", "apps.example.com", false)
		if out.Connection == nil {
			t.Fatal("expected Connection for a running instance with an FRP port")
		}
		if got, want := out.Connection.AppURL, "https://gpu-abcd.apps.example.com"; got != want {
			t.Errorf("app_url = %q; want %q", got, want)
		}
	})

	t.Run("raw instance has no app_url", func(t *testing.T) {
		out := instanceToPublic(mk("running", false), "frp.example.com", "apps.example.com", false)
		if out.Connection == nil {
			t.Fatal("expected Connection for a running raw instance")
		}
		if out.Connection.AppURL != "" {
			t.Errorf("raw instance app_url = %q; want empty", out.Connection.AppURL)
		}
	})

	t.Run("unconfigured AppsHost omits app_url (no malformed host)", func(t *testing.T) {
		out := instanceToPublic(mk("running", true), "frp.example.com", "", false)
		if out.Connection == nil {
			t.Fatal("expected Connection")
		}
		if out.Connection.AppURL != "" {
			t.Errorf("app_url with empty AppsHost = %q; want empty", out.Connection.AppURL)
		}
	})

	t.Run("non-running template instance has no connection/app_url", func(t *testing.T) {
		out := instanceToPublic(mk("booting", true), "frp.example.com", "apps.example.com", false)
		if out.Connection != nil {
			t.Errorf("expected nil Connection for a non-running instance, got %+v", out.Connection)
		}
	})

	// v74 (revising D6 surface-once) + GPU-59: on a plain (redacted) read the
	// persisted app_user is re-surfaced alongside app_url, but the password is
	// OMITTED — it returns only when includeCredentials is true (the
	// ?include=credentials opt-in on the single-instance GET).
	t.Run("plain read re-surfaces app_user but redacts the password", func(t *testing.T) {
		user, pass := "gpuai", "s3cret-pass"
		inst := mk("running", true)
		inst.AppAuthUser = &user
		inst.AppAuthPass = &pass
		out := instanceToPublic(inst, "frp.example.com", "apps.example.com", false)
		if out.Connection == nil {
			t.Fatal("expected Connection")
		}
		if out.Connection.AppUser != "gpuai" || out.Connection.AppPassword != "" {
			t.Errorf("app creds = %q/%q; want gpuai/<empty> (GPU-59 redaction)",
				out.Connection.AppUser, out.Connection.AppPassword)
		}
	})

	t.Run("includeCredentials restores the persisted password", func(t *testing.T) {
		user, pass := "gpuai", "s3cret-pass"
		inst := mk("running", true)
		inst.AppAuthUser = &user
		inst.AppAuthPass = &pass
		out := instanceToPublic(inst, "frp.example.com", "apps.example.com", true)
		if out.Connection == nil {
			t.Fatal("expected Connection")
		}
		if out.Connection.AppUser != "gpuai" || out.Connection.AppPassword != "s3cret-pass" {
			t.Errorf("app creds = %q/%q; want gpuai/s3cret-pass",
				out.Connection.AppUser, out.Connection.AppPassword)
		}
	})

	t.Run("pre-v74 rows omit app credentials", func(t *testing.T) {
		out := instanceToPublic(mk("running", true), "frp.example.com", "apps.example.com", true)
		if out.Connection.AppUser != "" || out.Connection.AppPassword != "" {
			t.Errorf("expected empty app creds for a row without them, got %q/%q",
				out.Connection.AppUser, out.Connection.AppPassword)
		}
	})

	// The credential must never leak onto a raw (no-template) instance even if
	// a row somehow carries values — not even with includeCredentials.
	t.Run("raw instance never surfaces app credentials", func(t *testing.T) {
		user, pass := "gpuai", "s3cret-pass"
		inst := mk("running", false)
		inst.AppAuthUser = &user
		inst.AppAuthPass = &pass
		out := instanceToPublic(inst, "frp.example.com", "apps.example.com", true)
		if out.Connection.AppUser != "" || out.Connection.AppPassword != "" {
			t.Errorf("raw instance leaked app creds %q/%q",
				out.Connection.AppUser, out.Connection.AppPassword)
		}
	})
}
