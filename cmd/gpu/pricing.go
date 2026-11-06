package main

import (
	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newPricingCmd builds `gpu pricing [--gpu-type X --region Y --tier Z --instant-boot]`.
// Catalog endpoint — public, no auth required.
func newPricingCmd() *cobra.Command {
	var (
		gpuType            string
		region             string
		tier               string
		instantBoot        bool
		community          bool
		includeUnavailable bool
		certified          bool
		deploymentType     string
	)
	cmd := &cobra.Command{
		Use:   "pricing",
		Short: "Show GPU pricing across regions and tiers",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			rows, err := deps.Client.ListPricing(cmd.Context(), apiclient.ListPricingOpts{
				GPUType: gpuType, Region: region, Tier: tier,
				IncludeUnavailable: includeUnavailable,
				Certified:          certified,
				DeploymentType:     deploymentType,
			})
			if err != nil {
				return mapClientError(err)
			}
			// Boot-speed filter is applied client-side: /v1/pricing keeps
			// boot class in its merge key, so per-offer rows still carry a
			// faithful InstantBoot bit and dropping slow rows here cannot
			// hide an instant offer the way post-aggregation filtering could.
			if instantBoot {
				kept := rows[:0]
				for _, r := range rows {
					if r.InstantBoot {
						kept = append(kept, r)
					}
				}
				rows = kept
			}
			// Community filter is client-side too: the community class rides
			// the /v1/pricing merge key, so per-offer rows carry a faithful
			// Community bit and dropping non-community rows here is safe. No
			// server-side community= query param exists.
			if community {
				kept := rows[:0]
				for _, r := range rows {
					if r.Community {
						kept = append(kept, r)
					}
				}
				rows = kept
			}
			return cliprint.Pricing(deps.Output, cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&gpuType, "gpu-type", "", "Filter by GPU type ID")
	cmd.Flags().StringVar(&region, "region", "", "Filter by region code")
	cmd.Flags().StringVar(&tier, "tier", "", "Filter by tier: on_demand | spot (suppliers may not offer spot; then --tier spot returns no rows)")
	cmd.Flags().BoolVar(&instantBoot, "instant-boot", false, "Only show capacity that boots in under a minute (hides ~3-5 min boot offerings)")
	cmd.Flags().BoolVar(&community, "community", false, "Only show community-class capacity (supplier/peer-hosted, softer SLA)")
	cmd.Flags().BoolVar(&includeUnavailable, "include-unavailable", false, "Include offers with zero current availability")
	cmd.Flags().BoolVar(&certified, "certified", false, "Only show offerings that can run the certified image (server-side certified_image filter)")
	cmd.Flags().StringVar(&deploymentType, "type", "", "Filter by deployment type: vm | container (container offerings can't run Docker-in-Docker)")
	return cmd
}
