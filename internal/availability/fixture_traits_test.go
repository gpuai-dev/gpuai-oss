package availability

import (
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Fixture provider names used across this package's tests. The plain names
// ("alpha", "bravo", "charlie", "delta", "echo") stay unregistered, so they
// carry the zero-value traits of a container-native adapter: instant boot,
// "container" deployment, certified-image capable, not disk-configurable, not
// featured. The descriptive names below register exactly the trait a test
// needs, so each test reads the classification it asserts from the same
// provider.Traits registry production uses.
const (
	fixtureSlow     = "slowco"     // slow boot, container-native
	fixtureVM       = "vmco"       // full VM handoff (slow boot, 15 min estimate)
	fixtureDisk     = "diskco"     // honors a requested container disk size
	fixtureMarket   = "marketco"   // a second disk-configurable marketplace
	fixtureFeatured = "featuredco" // eligible for curated featured rows
)

// communityProvider is the name provider.CommunityTier recognizes. GPU.ai Open
// Source ships no community adapter, so the tests register the traits one
// would declare for itself: supplier-owned hosts boot slowly and get a
// conservative create-to-ready estimate.
const communityProvider = "community"

func init() {
	provider.RegisterTraits(fixtureSlow, provider.Traits{SlowBoot: true})
	provider.RegisterTraits(fixtureVM, provider.Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 15 * time.Minute})
	provider.RegisterTraits(fixtureDisk, provider.Traits{ConfigurableDisk: true})
	provider.RegisterTraits(fixtureMarket, provider.Traits{ConfigurableDisk: true})
	provider.RegisterTraits(fixtureFeatured, provider.Traits{FeaturedPartner: true})
	provider.RegisterTraits(communityProvider, provider.Traits{SlowBoot: true, ProvisioningEstimate: 15 * time.Minute})
}
