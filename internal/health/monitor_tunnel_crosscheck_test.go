package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/db"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// statusProvider reports a fixed upstream status and counts the polls.
type statusProvider struct {
	notFoundProvider
	status string
	polls  atomic.Int32
}

func (p *statusProvider) GetStatus(context.Context, string, string) (*provider.InstanceStatus, error) {
	p.polls.Add(1)
	return &provider.InstanceStatus{Status: p.status}, nil
}

// A provider status of terminated/error is not proof a machine is gone. When
// a running instance still answers SSH through our own tunnel, the monitor
// must not fail it, and must decide that without the
// retry sleeps. Otherwise (a silent tunnel, a veto past its bound, a booting
// instance) the old failure path runs unchanged. Skips when DATABASE_TEST_URL
// is unset.
func TestHandleNonSpotFailure_TunnelCrossCheck(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	suffix := time.Now().Format("20060102150405.000000000")
	orgID, userID, err := pool.EnsureOrgAndUser(ctx, "test_org_xcheck_"+suffix, "test_user_xcheck_"+suffix, "x@example.com", "", "", "")
	if err != nil {
		t.Fatalf("ensure org+user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.PgxPool().Exec(bg, `DELETE FROM instance_events WHERE org_id = $1`, orgID)
		_, _ = pool.PgxPool().Exec(bg, `DELETE FROM instances WHERE org_id = $1`, orgID)
		_, _ = pool.PgxPool().Exec(bg, `DELETE FROM organizations WHERE organization_id = $1`, orgID)
	})

	for _, tc := range []struct {
		name       string
		status     string
		answers    bool
		vetoedFor  time.Duration // a veto already running this long; 0 for none
		wantStatus string
		wantOpen   bool
		wantProbes int
		wantPolls  int32 // provider polls in the retry loop
		port       int
	}{
		{"alive through the tunnel: kept", "running", true, 0, "running", true, 1, 0, 10042},
		{"silent through the tunnel: failed", "running", false, 0, "error", false, 1, nonSpotRetryCount, 10043},
		{"alive past the veto bound: failed", "running", true, tunnelVetoMaxDuration + time.Minute, "error", false, 1, nonSpotRetryCount, 10044},
		{"booting, alive through the tunnel: failed", "booting", true, 0, "error", false, 0, nonSpotRetryCount, 10045},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newTestID(t)
			port := tc.port
			inst := &db.Instance{
				InstanceID: id, OrgID: orgID, UserID: userID, UpstreamProvider: "flapprov", UpstreamID: "up-" + id,
				Hostname: id + ".example.com", GPUType: "RTX_3090", GPUCount: 1, Tier: "on_demand", Region: "us-east",
				PricePerHour: 0.16, UpstreamPricePerHour: 0.12, Status: tc.status, FRPRemotePort: &port,
			}
			if err := pool.CreateInstance(ctx, inst); err != nil {
				t.Fatalf("seed instance: %v", err)
			}
			var probed []string
			logs := &recordingHandler{}
			m := &Monitor{
				db: pool, registry: provider.NewRegistry(), logger: slog.New(logs), interval: time.Minute,
				tunnelProbeHost: "gw.test", nonSpotRetryEvery: time.Millisecond,
				probeTunnel: func(addr string) error {
					probed = append(probed, addr)
					if tc.answers {
						return nil
					}
					return errors.New("connection refused")
				},
			}
			if tc.wantOpen {
				// Keep the shipped 10s retry interval: a kept instance must
				// return without sleeping through the retries.
				m.nonSpotRetryEvery = 0
			}
			if tc.vetoedFor > 0 {
				m.tunnelVetoes.begin(id, time.Now().UTC().Add(-tc.vetoedFor))
			}
			prov := &statusProvider{notFoundProvider: notFoundProvider{name: "flapprov"}, status: "terminated"}
			start := time.Now()
			m.handleNonSpotFailure(ctx, inst, prov)
			elapsed := time.Since(start)

			if len(probed) != tc.wantProbes || (tc.wantProbes == 1 && probed[0] != fmt.Sprintf("gw.test:%d", tc.port)) {
				t.Errorf("tunnel probed %v; want the instance's gateway port %d time(s)", probed, tc.wantProbes)
			}
			if got := prov.polls.Load(); got != tc.wantPolls {
				t.Errorf("provider polled %d times in the retry loop; want %d", got, tc.wantPolls)
			}
			if tc.wantOpen && elapsed >= nonSpotRetryInterval {
				t.Errorf("a kept instance took %s; it must not sleep through the retries", elapsed)
			}
			if expired := logs.hasError(tunnelVetoExpiredMessage); expired != (tc.vetoedFor > 0) {
				t.Errorf("veto-expired ERROR logged=%v; want %v", expired, tc.vetoedFor > 0)
			}
			got, err := pool.GetInstance(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status %q; want %q", got.Status, tc.wantStatus)
			}
		})
	}
}
