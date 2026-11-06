package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
	"github.com/gpuai-dev/gpuai-oss/internal/cliprint"
)

// newInstancesCmd builds the `gpu instances` command group: create, list, get,
// update, delete (alias terminate), ssh and open.
func newInstancesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instances",
		Short: "Manage GPU instances",
	}
	cmd.AddCommand(
		newInstancesCreateCmd(),
		newInstancesListCmd(),
		newInstancesGetCmd(),
		newInstancesUpdateCmd(),
		newInstancesDeleteCmd(),
		newInstancesSSHCmd(),
		newInstancesOpenCmd(),
	)
	return cmd
}

func newInstancesListCmd() *cobra.Command {
	var (
		limit  int
		status string
		all    bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your GPU instances",
		Long: "Lists the organization's non-terminated instances by default. Use --status to " +
			"filter by state, or --all to include the full terminated history.",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			if all {
				status = "all"
			}
			page, err := deps.Client.ListInstances(cmd.Context(), apiclient.ListInstancesOpts{Limit: limit, Status: status})
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.Instances(deps.Output, cmd.OutOrStdout(), page.Data)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Max instances to fetch (1-200)")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: allocating | starting | running | stopping | stopped | terminated | error | all (default: all non-terminated; unreachable instances list under running)")
	cmd.Flags().BoolVar(&all, "all", false, "Include terminated instances (shorthand for --status=all)")
	return cmd
}

func newInstancesGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <instance-id>",
		Short: "Show details for one instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			// `gpu instances get` renders the App Login line, so it opts in
			// to the credential read (?include=credentials).
			inst, err := deps.Client.GetInstanceWithCredentials(cmd.Context(), args[0])
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.InstanceDetail(deps.Output, cmd.OutOrStdout(), inst)
		},
	}
}

// parsePortSpecs projects --port PORT/PROTOCOL entries plus the --app-port
// shorthand into the API's port list. --app-port marks (or adds) the primary
// http port.
func parsePortSpecs(specs []string, appPort int) ([]apiclient.InstancePort, error) {
	var out []apiclient.InstancePort
	sawApp := false
	for _, s := range specs {
		part := strings.SplitN(s, "/", 2)
		if len(part) != 2 {
			return nil, fmt.Errorf("--port %q must be PORT/PROTOCOL (e.g. 8000/http)", s)
		}
		n, err := strconv.Atoi(part[0])
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("--port %q has an invalid port number", s)
		}
		proto := strings.ToLower(part[1])
		if proto != "http" && proto != "tcp" {
			return nil, fmt.Errorf("--port %q protocol must be http or tcp", s)
		}
		p := apiclient.InstancePort{Port: n, Protocol: proto}
		if appPort != 0 && n == appPort {
			if proto != "http" {
				return nil, fmt.Errorf("--app-port %d must be an http port", appPort)
			}
			p.Primary = true
			sawApp = true
		}
		out = append(out, p)
	}
	if appPort != 0 && !sawApp {
		out = append(out, apiclient.InstancePort{Port: appPort, Protocol: "http", Primary: true})
	}
	return out, nil
}

func newInstancesCreateCmd() *cobra.Command {
	var (
		gpuType            string
		gpuCount           int
		tier               string
		region             string
		name               string
		sshKeyIDs          []string
		environment        string
		image              string
		imageEnv           map[string]string
		registryCredential string
		entrypointArgs     []string
		cmdArgs            []string
		portSpecs          []string
		appPort            int
		offeringID         string
		maxPrice           float64
		viewedPrice        float64
		diskGB             int
		autoTerminateHours int
		waitTimeout        time.Duration
	)
	cmd := &cobra.Command{
		Use:         "create",
		Short:       "Create a new GPU instance",
		Annotations: mutatingCmd,
		Long: "Creates a GPU instance asynchronously. The CLI polls the operation FSM until it " +
			"reaches succeeded or fails (5-minute deadline by default). Use --wait-timeout=0 " +
			"to skip polling.\n\n" +
			"Placement & price: with --offering-id the launch is PINNED — the exact offering " +
			"and its listed rate, or an explicit error; nothing is substituted. Without it the " +
			"launch is UNPINNED — the cheapest capable capacity wins, and the final rate is " +
			"bounded by the price cap, which defaults to the cheapest currently-listed matching " +
			"price. Placement and any boot rescue never exceed the cap; capacity priced above it " +
			"is skipped. Adjust with --max-price, or pass --max-price 0 to launch uncapped.\n\n" +
			"A pin locks the machine, not the rate — the catalog row is a quote and the supplier's " +
			"price can move under it. Pass --viewed-price with the rate you decided on and a launch-time price " +
			"above it fails with the new price instead of proceeding at it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if gpuType == "" {
				return fmt.Errorf("--type/-t is required (e.g., --type h100_sxm)")
			}
			// Client-side mirror of the server's exclusivity gate, for a
			// friendlier failure than a round-trip 422.
			if image != "" && environment != "" {
				return fmt.Errorf("--image and --environment are mutually exclusive — a custom image is its own environment")
			}
			if len(imageEnv) > 0 && image == "" {
				return fmt.Errorf("--env requires --image")
			}
			if image == "" && (registryCredential != "" || len(entrypointArgs) > 0 || len(cmdArgs) > 0 || len(portSpecs) > 0 || appPort != 0) {
				return fmt.Errorf("--registry-credential/--entrypoint/--cmd/--port/--app-port require --image")
			}
			if maxPrice < 0 {
				return fmt.Errorf("--max-price must be >= 0 (0 = launch uncapped)")
			}
			if viewedPrice < 0 {
				return fmt.Errorf("--viewed-price must be >= 0 (0 = don't send a viewed price)")
			}
			deps := depsFromCtx(cmd.Context())
			req := apiclient.CreateInstanceRequest{
				GPUType:   gpuType,
				GPUCount:  gpuCount,
				Tier:      tier,
				Region:    region,
				SSHKeyIDs: sshKeyIDs,
			}
			if name != "" {
				req.Name = &name
			}
			if image != "" {
				req.Image = &image
				req.Env = imageEnv
				if registryCredential != "" {
					req.RegistryCredentialID = &registryCredential
				}
				// cobra can't distinguish "flag absent" from "no uses" for
				// arrays, so an empty slice means absent (use the image's
				// own). The docker-CLI clear spelling `--entrypoint ""`
				// (a single empty element) maps to the API's clear idiom —
				// an explicit empty array — because the API rejects empty
				// argv ELEMENTS (they render as `exec ''` and kill the
				// workload).
				if len(entrypointArgs) == 1 && entrypointArgs[0] == "" {
					req.Entrypoint = &[]string{}
				} else if len(entrypointArgs) > 0 {
					req.Entrypoint = &entrypointArgs
				}
				if len(cmdArgs) == 1 && cmdArgs[0] == "" {
					req.Cmd = &[]string{}
				} else if len(cmdArgs) > 0 {
					req.Cmd = &cmdArgs
				}
				ports, perr := parsePortSpecs(portSpecs, appPort)
				if perr != nil {
					return perr
				}
				req.Ports = ports
			}
			envWire := ""
			if environment != "" {
				// CLI uses hyphenated class spellings ("raw-vm", "provider-template");
				// the API wire values are snake_case. certified and its framework +
				// version forms (certified:pytorch, certified:cuda-devel,
				// certified:pytorch@2.13 and the version picker) pass through
				// verbatim: framework ids and versions carry no hyphen→snake mapping.
				// raw-vm carries an optional OS suffix (raw-vm:ubuntu-24.04) that must
				// survive the class rename, so match the prefix, not just the bare word.
				envWire = environment
				switch {
				case envWire == "raw-vm":
					envWire = "raw_vm"
				case strings.HasPrefix(envWire, "raw-vm:"):
					envWire = "raw_vm:" + strings.TrimPrefix(envWire, "raw-vm:")
				case envWire == "provider-template":
					envWire = "provider_template"
				}
				req.Environment = &envWire
			}
			if offeringID != "" {
				req.OfferingID = &offeringID
			}
			if diskGB > 0 {
				req.DiskGB = &diskGB
			}
			// Send the runtime limit only when one was asked for. The server
			// refuses an explicit 0 (it is a broken request, not "no limit"),
			// so the flag's own zero value must never reach the wire — this is
			// the one place that distinction is enforced for CLI callers.
			if autoTerminateHours > 0 {
				req.AutoTerminateHours = &autoTerminateHours
			}

			// Placement-mode contract. A pinned launch locks the exact offering
			// (and its rate); an unpinned launch is price-sorted placement whose
			// final rate is only bounded by a max-price cap — so default the cap
			// to the currently-listed cheapest matching price whenever the
			// customer didn't choose one. --max-price 0 opts out explicitly.
			// Messages go to stderr so -o json stdout stays parseable.
			//
			// --viewed-price applies to EVERY launch mode, pinned or not: the
			// engine's check is not pin-gated, so an unpinned launch that
			// passes the flag also fails loudly (price_changed) when the
			// launch-time quote is more than a cent above the price the caller
			// saw, instead of silently launching at the new number. Explicit-only,
			// never derived from the catalog: omitting it keeps the pre-flag
			// behavior exactly.
			if viewedPrice > 0 {
				req.ViewedPricePerHour = &viewedPrice
			}
			switch {
			case offeringID != "":
				if maxPrice > 0 {
					req.MaxPricePerHour = &maxPrice
				}
				// A pin locks the machine, not the rate — the row is a quote and
				// the supplier price can move under it; the viewed-price guard
				// set above is what fails the launch loudly on a re-quote.
				if viewedPrice > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"Pinned launch: offering %s — no substitute capacity is used, and the launch fails with the new price if the rate has moved above %s/hr.\n",
						offeringID, fmtUSD(viewedPrice))
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"Pinned launch: offering %s — its listed rate is locked; no substitute capacity is used.\n", offeringID)
				}
			case cmd.Flags().Changed("max-price") && maxPrice > 0:
				req.MaxPricePerHour = &maxPrice
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Unpinned launch: cheapest capable capacity, capped at %s/hr — placement and any boot rescue never exceed the cap.\n",
					fmtUSD(maxPrice))
			case cmd.Flags().Changed("max-price"):
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Unpinned launch with no price cap: the final rate is set at placement — check price_per_hour on the created instance.")
			default:
				if quote, ok := quoteCheapest(cmd.Context(), deps.Client, gpuType, envWire, region, tier, gpuCount, diskGB); ok {
					req.MaxPricePerHour = &quote
					fmt.Fprintf(cmd.ErrOrStderr(),
						"Unpinned launch: price cap defaulted to %s/hr (today's cheapest matching capacity) — placement and any boot rescue never exceed it.\n"+
							"Capacity above the cap is skipped; adjust with --max-price, or pass --max-price 0 to launch uncapped.\n",
						fmtUSD(quote))
				} else {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"Unpinned launch: no matching capacity is listed right now to derive a price cap — proceeding uncapped (final rate is set at placement).")
				}
			}
			op, opID, err := deps.Client.CreateInstance(cmd.Context(), req)
			if err != nil {
				return mapClientError(err)
			}
			if opID == "" {
				opID = op.OperationID
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Submitted: operation %s (state=%s) — polling...\n", opID, op.State)
			if waitTimeout == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), opID)
				return nil
			}
			// bounded poll deadline — CLI never infinite-loops.
			finalOp, err := deps.Client.PollOperation(cmd.Context(), opID, waitTimeout)
			if err != nil {
				return mapClientError(err)
			}
			if finalOp.State != "succeeded" {
				detail := "no detail"
				if finalOp.Error != nil {
					detail = finalOp.Error.Detail
				}
				// Key off the stable error CODE, not the free-text detail.
				// The detail substring was the only signal before
				// `price_changed` existed, and it silently stops matching the
				// moment the engine's message is reworded. Kept as a fallback
				// so an older server (still emitting operation_failed) keeps
				// producing the hint.
				code := ""
				if finalOp.Error != nil {
					code = finalOp.Error.Code
				}
				if code == "price_changed" || strings.Contains(detail, "exceeds maximum") {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"The price moved above the price this launch was approved for. "+
							"Nothing was provisioned. Re-run to accept the "+
							"current price (if you passed --viewed-price, update it to the new "+
							"number first — re-running unchanged will fail again), or set "+
							"--max-price to the number you're willing to pay.")
				}
				return fmt.Errorf("instance create operation %s: %s", finalOp.State, detail)
			}
			if finalOp.ResourceID == nil || *finalOp.ResourceID == "" {
				return fmt.Errorf("operation succeeded but resource_id missing — check `gpu instances list`")
			}
			// The post-create InstanceDetail prints the App Login for
			// console-provisioned instances — opt in to the credential read.
			inst, err := deps.Client.GetInstanceWithCredentials(cmd.Context(), *finalOp.ResourceID)
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.InstanceDetail(deps.Output, cmd.OutOrStdout(), inst)
		},
	}
	cmd.Flags().StringVarP(&gpuType, "type", "t", "", "GPU type ID (e.g., h100_sxm) — required")
	cmd.Flags().IntVar(&gpuCount, "count", 1, "Number of GPUs")
	cmd.Flags().StringVar(&tier, "tier", "on_demand", "Tier: on_demand | spot (spot is soft-deprecated and suppliers may not offer it)")
	cmd.Flags().StringVar(&region, "region", "", "Region (e.g., us-east); empty = any")
	cmd.Flags().StringVar(&name, "name", "", "Optional instance name")
	cmd.Flags().StringSliceVar(&sshKeyIDs, "ssh-key-id", nil, "SSH key ID to inject (may repeat)")
	cmd.Flags().StringVar(&environment, "environment", "",
		"Launch environment: certified (the certified image) | certified:<framework> "+
			"(pytorch, cuda-devel, jupyter — a framework layer on the certified base) | "+
			"certified:<framework>@<version> (e.g. certified:pytorch@2.13) | "+
			"raw-vm (root VM access) | raw-vm:<os> (e.g. raw-vm:ubuntu-24.04) | "+
			"provider-template (the supplier's own image); empty = the supplier default. "+
			"Run gpu environments for the catalog")
	cmd.Flags().StringVar(&image, "image", "",
		"Launch from your own container image (e.g. ghcr.io/acme/trainer:v1). Must be public "+
			"(or private with --registry-credential) and linux/amd64 — verified before anything is provisioned, and the launch is pinned "+
			"to the exact digest verified. The image's own ENTRYPOINT/CMD starts after SSH setup. "+
			"Mutually exclusive with --environment")
	cmd.Flags().StringToStringVar(&imageEnv, "env", nil,
		"Container env var KEY=VALUE for a --image launch (may repeat); PUBLIC_KEY and GPUAI_* are reserved")
	cmd.Flags().StringVar(&registryCredential, "registry-credential", "",
		"Stored registry-credential ID for pulling a PRIVATE --image (see gpu registry-credentials)")
	cmd.Flags().StringArrayVar(&entrypointArgs, "entrypoint", nil,
		"Override the image ENTRYPOINT; one argv element per use (e.g. --entrypoint python --entrypoint serve.py). Replaces the image CMD too unless --cmd is given; a single --entrypoint \"\" clears the image's ENTRYPOINT (docker-style)")
	cmd.Flags().StringArrayVar(&cmdArgs, "cmd", nil,
		"Override the image CMD; one argv element per use; a single --cmd \"\" clears the image's CMD")
	cmd.Flags().StringArrayVar(&portSpecs, "port", nil,
		"Expose a container port as PORT/PROTOCOL (http|tcp), e.g. --port 8000/http (may repeat)")
	cmd.Flags().IntVar(&appPort, "app-port", 0,
		"HTTP port served at your instance's app URL (basic-auth protected; needs the server's optional tunnel). Added to --port automatically")
	cmd.Flags().StringVar(&offeringID, "offering-id", "",
		"Pin the launch to one exact offering ID; empty = price-sorted placement")
	cmd.Flags().Float64Var(&maxPrice, "max-price", 0,
		"Cap the $/hr (whole node); placement and any boot rescue never exceed it. "+
			"On an unpinned launch the cap DEFAULTS to the cheapest currently-listed matching "+
			"price; pass 0 explicitly to launch uncapped. If the cheapest available is above "+
			"the cap, the launch fails with the current price so you can re-quote.")
	cmd.Flags().Float64Var(&viewedPrice, "viewed-price", 0,
		"The $/hr you decided to launch at (pair with --offering-id). A pin locks the "+
			"machine, not the rate: if the price has moved more than a cent above this by launch "+
			"time the launch fails with the new price instead of proceeding at it. 0 = don't send "+
			"one, which accepts the launch-time price (the default, unchanged behavior). This is "+
			"NOT a price cap — it can only refuse a launch, never authorize a pricier one; use "+
			"--max-price for that.")
	cmd.Flags().IntVar(&diskGB, "disk", 0,
		"Instance filesystem size in GB (10-8192). 0 = the platform default (100 GB); placement "+
			"always avoids machines whose storage cannot fit the size the launch will request, "+
			"default included. Disk beyond the included 100 GB is priced at the offering's "+
			"disk_price_per_gb_hour (see gpu pricing), folded into the hourly rate. Setting it "+
			"narrows to capacity that can honor it — check disk_configurable in gpu pricing; an "+
			"unhonorable request fails rather than launching with a different size.")
	cmd.Flags().IntVar(&autoTerminateHours, "auto-terminate-hours", 0,
		"Terminate this instance automatically after N hours of runtime (1-720; 0 = no limit, "+
			"the default). The clock starts when the instance reaches running, not when this command "+
			"runs — provisioning is not counted — and gpu instances get shows the "+
			"resulting deadline. Enforced by the platform, not by this CLI: it still fires if this "+
			"process is gone, which is the point for unattended runs.")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Minute,
		"How long to poll the operation; 0 = don't poll, just print operation_id")
	return cmd
}

// quoteCheapest resolves the cheapest currently-listed offering matching the
// create filters and returns its disk-inclusive hourly price — the value an
// unpinned launch defaults its price cap to. Mirrors the server's placement
// eligibility: GPU type/count/region/tier (larger nodes qualify, as in
// placement's GPUCount >= match), the certified / raw-vm environment classes,
// and (when --disk was passed) configurable-disk capacity with the extra GB
// folded in at the row's rate. A lookup failure returns ok=false — the launch
// proceeds uncapped rather than failing on a quote hiccup.
//
// The returned quote is rounded UP to the next whole cent. The server folds
// disk into the wholesale basis and cent-rounds the total once, while this
// client adds an un-rounded per-GB coefficient to an already-rounded row
// price — the two can diverge by strictly less than a cent in either
// direction, and the server compares with strict '>'. Ceiling the cap makes
// it provably >= the server's cent-aligned price for the quoted row, so the
// default cap can never spuriously price-fail its own quote. The row's
// instance_disk_gb is used as the included-disk baseline; the server prices
// relative to its own included-disk setting (same 100 GB default).
func quoteCheapest(ctx context.Context, client *apiclient.Client, gpuType, envWire, region, tier string, gpuCount, diskGB int) (float64, bool) {
	opts := apiclient.ListPricingOpts{GPUType: gpuType, Region: region, Tier: tier}
	if strings.HasPrefix(envWire, "certified") {
		opts.Certified = true
	}
	if strings.HasPrefix(envWire, "raw_vm") {
		opts.DeploymentType = "vm"
	}
	rows, err := client.ListPricing(ctx, opts)
	if err != nil {
		return 0, false
	}
	best, found := 0.0, false
	for _, r := range rows {
		if r.GPUCount < gpuCount || r.Available <= 0 {
			continue
		}
		price := r.PricePerHour
		if diskGB > 0 {
			if !r.DiskConfigurable {
				continue
			}
			if extra := diskGB - r.InstanceDiskGB; extra > 0 {
				price += float64(extra) * r.DiskPricePerGBHour
			}
		}
		if !found || price < best {
			best, found = price, true
		}
	}
	if !found {
		return 0, false
	}
	// Ceil to the next cent (see doc comment). The epsilon keeps already
	// cent-aligned values put despite binary float noise (0.1*100 can read
	// 10.000000000000002).
	return math.Ceil(best*100-1e-9) / 100, true
}

// fmtUSD renders an hourly rate with enough precision for sub-cent
// prices ($0.0956) without noisy trailing zeros on cent-scale
// prices ($0.34, $2).
func fmtUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return "$" + s
}

func newInstancesUpdateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:         "update <instance-id>",
		Short:       "Update an instance (rename)",
		Annotations: mutatingCmd,
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("name") {
				return fmt.Errorf("--name is required (renaming is the only update the API supports)")
			}
			deps := depsFromCtx(cmd.Context())
			inst, err := deps.Client.PatchInstance(cmd.Context(), args[0], name)
			if err != nil {
				return mapClientError(err)
			}
			return cliprint.InstanceDetail(deps.Output, cmd.OutOrStdout(), inst)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "New instance name")
	return cmd
}

func newInstancesDeleteCmd() *cobra.Command {
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:         "delete <instance-id>",
		Aliases:     []string{"terminate"},
		Short:       "Terminate an instance",
		Annotations: mutatingCmd,
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := depsFromCtx(cmd.Context())
			op, opID, err := deps.Client.DeleteInstance(cmd.Context(), args[0])
			if err != nil {
				return mapClientError(err)
			}
			if opID == "" {
				opID = op.OperationID
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Submitted: terminate operation %s (state=%s) — polling...\n", opID, op.State)
			if waitTimeout == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), opID)
				return nil
			}
			finalOp, err := deps.Client.PollOperation(cmd.Context(), opID, waitTimeout)
			if err != nil {
				return mapClientError(err)
			}
			if finalOp.State != "succeeded" {
				detail := "no detail"
				if finalOp.Error != nil {
					detail = finalOp.Error.Detail
				}
				return fmt.Errorf("terminate operation %s: %s", finalOp.State, detail)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Instance %s terminated.\n", args[0])
			return nil
		},
	}
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 60*time.Second,
		"How long to poll the terminate operation; 0 = don't poll")
	return cmd
}
