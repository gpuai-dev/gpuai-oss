package provision

import (
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// Fixture provider vocabulary for this package's tests. Each name stands for
// a CLASS of adapter, not a real supplier; its traits are registered here so
// every test that uses a name gets the same classification:
//
//   - podco: container-native, honors disk size, takes template and customer
//     images, and starts the bootstrap as ENTRYPOINT arguments.
//   - marketco: container-native marketplace, honors disk size, takes
//     template and customer images.
//   - fixedimageco: fixed provider-managed image (cannot take the Certified
//     Image or a custom image), honors disk size.
//   - templateonlyco: container-native, slow first boot, takes template
//     images but not customer images.
//   - vmco, metalco, cloudvmco, baremetalco: slow-boot VM / bare-metal
//     adapters that also expose the raw-vm environment class.
//   - vmimageco: fast-boot VM adapter that exposes the raw-vm class.
//   - startupvmco: slow-boot VM adapter without the raw-vm class.
//
// Any other fixture name (stub, alpha, charlie, ...) is an unregistered,
// plain container-native adapter.
func init() {
	provider.RegisterTraits("podco", provider.Traits{ConfigurableDisk: true})
	provider.RegisterTraits("marketco", provider.Traits{ConfigurableDisk: true})
	provider.RegisterTraits("fixedimageco", provider.Traits{NonNormalizable: true, ConfigurableDisk: true})
	provider.RegisterTraits("templateonlyco", provider.Traits{SlowBoot: true, ProvisioningEstimate: 20 * time.Minute})
	provider.RegisterTraits("vmco", provider.Traits{SlowBoot: true, VM: true, FeaturedPartner: true, ProvisioningEstimate: 5 * time.Minute})
	provider.RegisterTraits("metalco", provider.Traits{SlowBoot: true, VM: true, FeaturedPartner: true, ProvisioningEstimate: 15 * time.Minute})
	provider.RegisterTraits("cloudvmco", provider.Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 10 * time.Minute})
	provider.RegisterTraits("baremetalco", provider.Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 10 * time.Minute})
	provider.RegisterTraits("vmimageco", provider.Traits{VM: true})
	provider.RegisterTraits("startupvmco", provider.Traits{SlowBoot: true, VM: true, ProvisioningEstimate: 5 * time.Minute})

	for _, name := range []string{"podco", "marketco", "templateonlyco"} {
		customImageProviders[name] = true
	}
	for _, name := range []string{"podco", "marketco"} {
		customerImageProviders[name] = true
	}
	entrypointArgProviders["podco"] = true
	for _, name := range []string{"vmco", "metalco", "cloudvmco", "vmimageco", "baremetalco"} {
		rawVMProviders[name] = true
	}
}
