package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// statusReport is the machine-readable shape of `fuse status --output json`.
type statusReport struct {
	Context      string                 `json:"context"`
	BaseURL      string                 `json:"base_url"`
	Master       bool                   `json:"master"`
	ActiveHost   *fuse.Host             `json:"active_host,omitempty"`
	HostCount    int                    `json:"host_count"`
	HostCounted  bool                   `json:"host_count_complete"`
	Environments map[string]int         `json:"environments_by_state"`
	Scope        string                 `json:"environment_scope"` // "host" | "fleet"
	Endpoints    []fuse.EnvironmentInfo `json:"running_endpoints,omitempty"`
}

// newStatusCmd implements `fuse status`: a single glanceable view of the
// active context, active host capacity, and environment counts, so an
// operator does not need to run `hosts get` + `environment list` separately.
func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the active context, host, and environment summary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, cur, err := app.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			report := statusReport{
				Context: cur.Name,
				BaseURL: cur.BaseURL,
				Master:  cur.Master,
			}

			hostPage, err := cl.Hosts.ListPage(ctx, fuse.ListHostsOptions{})
			if err != nil {
				return friendly(err)
			}
			report.HostCount = len(hostPage.Hosts)
			report.HostCounted = hostPage.NextCursor == ""

			if cur.ActiveHost != "" {
				h, err := cl.Hosts.Get(ctx, cur.ActiveHost)
				if err != nil {
					return friendly(err)
				}
				report.ActiveHost = h
				report.Scope = "host"
			} else {
				report.Scope = "fleet"
			}

			envOpts := fuse.ListEnvironmentsOptions{HostID: cur.ActiveHost}
			envs, err := cl.Environments.List(ctx, envOpts)
			if err != nil {
				return friendly(err)
			}
			report.Environments = countByState(envs)
			for _, e := range envs {
				if e.State == fuse.StateRunning && e.URL != "" {
					report.Endpoints = append(report.Endpoints, e)
				}
			}

			if app.isJSON() {
				return printJSON(report)
			}
			renderStatus(report)
			return nil
		},
	}
}

// countByState tallies environments by lifecycle state.
func countByState(envs []fuse.EnvironmentInfo) map[string]int {
	counts := make(map[string]int)
	for _, e := range envs {
		counts[e.State]++
	}
	return counts
}

func renderStatus(r statusReport) {
	master := ""
	if r.Master {
		master = styleFaint.Render(" (master)")
	}
	fmt.Printf("%s %s%s\n", styleHeader.Render("context"), r.Context, master)
	fmt.Printf("  %s\n", styleFaint.Render(r.BaseURL))
	fmt.Println()

	if r.ActiveHost != nil {
		h := r.ActiveHost
		fmt.Printf("%s %s  %s\n", styleHeader.Render("active host"), h.ID, stateStyle(h.State))
		fmt.Printf("  cpus %d/%d   ram %d/%d mb   storage %d/%d gb   vms %d/%d\n",
			h.Allocated.CPUs, h.Capacity.CPUs,
			h.Allocated.RamMB, h.Capacity.RamMB,
			h.Allocated.StorageGB, h.Capacity.StorageGB,
			h.Allocated.VMCount, h.Capacity.VMCount)
	} else {
		fmt.Printf("%s %s\n", styleHeader.Render("active host"), styleFaint.Render("none (fleet-wide scope; run `fuse host <id>` to scope)"))
	}
	if r.HostCounted {
		fmt.Printf("  %s\n", styleFaint.Render(fmt.Sprintf("%d host(s) registered", r.HostCount)))
	} else {
		fmt.Printf("  %s\n", styleFaint.Render(fmt.Sprintf("%d+ host(s) registered", r.HostCount)))
	}
	fmt.Println()

	fmt.Printf("%s (%s scope)\n", styleHeader.Render("environments"), r.Scope)
	if len(r.Environments) == 0 {
		fmt.Printf("  %s\n", styleFaint.Render("none"))
	} else {
		states := make([]string, 0, len(r.Environments))
		for s := range r.Environments {
			states = append(states, s)
		}
		sort.Strings(states)
		parts := make([]string, 0, len(states))
		for _, s := range states {
			parts = append(parts, fmt.Sprintf("%s %s", stateStyle(s), styleFaint.Render(fmt.Sprintf("x%d", r.Environments[s]))))
		}
		fmt.Printf("  %s\n", strings.Join(parts, "   "))
	}

	if len(r.Endpoints) > 0 {
		fmt.Println()
		fmt.Println(styleHeader.Render("running endpoints"))
		for _, e := range r.Endpoints {
			fmt.Printf("  %s  %s\n", e.ID, e.URL)
		}
	}
}
