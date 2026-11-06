package availability

import (
	"strconv"
	"time"
)

// Launch-outcome breaker (GPU-108 follow-up).
//
// The poller's Breaker trips when a provider's availability API ERRORS. It is
// blind to the failure mode that burned 55 launches in 2026-09: one provider's
// stock endpoint stayed healthy and kept advertising A100 capacity its
// scheduler could not place, so every create was accepted and died in ERROR
// ~25s later. No API error, no trip — the catalog kept selling a SKU that
// could not boot.
//
// The launch-outcome breaker closes that gap by scoring what actually
// happened to launches, not what the provider claims: the provision engine
// records a failure when an upstream reports terminated/error before ever
// running, and a success when an upstream first reports running. It reuses
// the same Breaker state machine, keyed per (provider, gpu_type) via
// LaunchKey rather than per provider — that provider's A100s were dead while
// its H100s provisioned fine, and suppressing the whole supplier for one
// dead SKU would be over-broad.
//
// Consumers:
//   - the availability poller drops Sheddable keys (tripped AND inside the
//     cooldown) from the customer catalog — the row vanishes, mirroring the
//     zero-stock UX, so customers cannot pin an offering our own outcomes
//     say is dead. The hiding is deliberately cooldown-BOUNDED: a hidden
//     SKU generates no traffic, so nothing could ever heal it — after the
//     cooldown the row returns and real launches become the probes;
//   - the provision engine's candidate selection calls Allow per offering,
//     so a tripped SKU is skipped at placement and a cooled-down one gets
//     half-open probe launches — which the upstream-terminal failover
//     (#846) rescues invisibly if the probe dies. Between cooldown expiry
//     and the first recorded outcome every concurrent matching launch is
//     admitted (Allow in half-open is not a single-probe latch); that
//     ~one-boot-cycle herd is bounded by the immediate re-trip on the
//     first failure, and #846 rescues each admitted launch.
//
// Both consumers share ONE Breaker instance (wired in cmd/gpuctl/deps.go);
// state is in-memory and resets on restart, the same deliberate trade-off as
// the poller breaker.
const (

	// The windowed-rate trip rule (see NewRateBreaker), derived by replaying
	// 3,924 real prod launch outcomes over the 14 days to 2026-09-17, keyed per
	// (provider, gpu_type, gpu_count).
	//
	// Trip when at least MinSamples of the last Window outcomes are recorded and
	// the failure rate over them is >= FailRate.
	//
	//   The rule has to separate two measured neighbours: trip provider A's
	//   a100_80gb x1 at 79.2% while sparing provider B's a100_80gb x4 at
	//   63.3%. Everything else is comfortably outside that band — the other
	//   dead shapes sit at 80-100% (l40 x4, b200 x8, rtx_a6000 x2 at 100%,
	//   h100_pcie x1 at 88.9%, l40 x8 at 80%) and the fleet-wide boot-failure
	//   baseline is 16.8%. Provider A's l40 x1, at 4.3%, is the one the old
	//   per-model key suppressed as collateral.
	//
	//   Window 20, not 10, because the window QUANTIZES the achievable rate. At
	//   window 10 the only thresholds are multiples of 10%: 0.70 trips a 63.3%
	//   shape as soon as one window happens to hold 7 failures, and 0.80 stops
	//   tripping the 79.2% one. Neither separates the pair. Window 20 gives 5%
	//   steps, so 0.75 trips at 15/20 = 75% — above 63.3% (12-13/20) and below
	//   79.2% (15-16/20) with margin on both sides. This was invisible in the
	//   replay of real ordered sequences, which never happened to realise the
	//   unlucky window; a steady-state test found it.
	//
	//   MinSamples 5 over 4: on the 14-day sample both trip the same keys with
	//   zero healthy shapes caught, and 4 reacts one launch sooner. 5 wins on
	//   the arithmetic the sample cannot show — 4 samples means 3 failures trip
	//   a key, which a genuinely 40%-failure shape hits ~18% of the time by
	//   chance, against ~8.7% for 4-of-5. Halving the false-trip probability is
	//   worth one launch.
	DefaultLaunchBreakerWindow     = 20
	DefaultLaunchBreakerMinSamples = 5
	DefaultLaunchBreakerFailRate   = 0.75

	// DefaultLaunchBreakerCooldown is how long a tripped SKU stays fully
	// suppressed before half-open probe launches are allowed. Sized to the
	// observed flap cadence of the A100 incident above (capacity came and
	// went over tens of minutes), not the 60s API-breaker default — a
	// capacity drought does not clear in a minute.
	DefaultLaunchBreakerCooldown = 10 * time.Minute
)

// LaunchKey is the launch-outcome breaker's state key: one circuit per
// (provider, canonical gpu_type, gpu_count). The separator cannot appear in a
// registry provider name or a canonical gpu_type, so keys never collide.
//
// gpu_count is part of the key because placement succeeds or fails per SHAPE,
// not per GPU model. Measured on prod over the 14 days to 2026-09-17,
// on one provider:
//
//	l40 x1          2/46 failed   (4.3% — healthy, sells fine)
//	l40 x4         10/10 failed   (100%)
//	l40 x8          4/5  failed   (80%)
//	b200 x8         5/5  failed   (100%)
//	h100_pcie x1    6/9  failed   (67%)
//
// Keyed only on (provider, gpu_type), those l40 rows share ONE circuit, and
// neither outcome is acceptable: the x1 successes keep resetting the counter so
// the dead x4 keeps selling, or the x4 failures trip the circuit and suppress a
// 4.3%-failure SKU that customers are buying happily. A 4-GPU placement needs
// four free GPUs on one host; a 1-GPU placement needs one. Those are different
// questions about the provider's fleet and deserve different circuits.
//
// This only makes the breaker more PRECISE. Splitting one circuit into several
// cannot newly suppress capacity that a coarser key left healthy — each shape
// now trips on its own evidence.
func LaunchKey(providerName, gpuType string, gpuCount int) string {
	// An unset count normalizes to 1 rather than minting an "x0" circuit.
	// The two sides of this breaker must agree on the key or it silently stops
	// working: placement reads it from GPUOffering.GPUCount (always >= 1) while
	// the outcome recorders read it from the instance row. A caller that leaves
	// the count zero would record failures against a key no Allow ever looks
	// up — the circuit would never trip and nothing would report an error.
	if gpuCount < 1 {
		gpuCount = 1
	}
	return providerName + "|" + gpuType + "|x" + strconv.Itoa(gpuCount)
}
