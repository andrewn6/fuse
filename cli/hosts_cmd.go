package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// newHostsCmd implements `fuse hosts list` (and bare `fuse hosts`).
func newHostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hosts",
		Short:   "List compute hosts in the active orchestrator",
		Args:    cobra.NoArgs,
		RunE:    runHostsList,
		Aliases: []string{"host-list"},
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List compute hosts",
		Args:    cobra.NoArgs,
		RunE:    runHostsList,
	})
	// The `hosts`/`host` split is a trap: everyone reaches for
	// `fuse hosts register` first. Register the same management
	// subcommands under both so either name works.
	cmd.AddCommand(hostManagementCommands()...)
	return cmd
}

// hostManagementCommands builds the per-host management subcommands
// (register, get, cordon, uncordon, remove, metrics). They are attached
// to both `fuse host` and `fuse hosts` so neither name is a dead end.
func hostManagementCommands() []*cobra.Command {
	return []*cobra.Command{
		newHostRegisterCmd(),
		newHostGetCmd(),
		newHostActionCmd("cordon", "Mark a host unschedulable", (*fuse.HostsService).Cordon),
		newHostActionCmd("uncordon", "Mark a host schedulable again", (*fuse.HostsService).Uncordon),
		newHostRemoveCmd(),
		newHostMetricsCmd(),
	}
}

func runHostsList(cmd *cobra.Command, _ []string) error {
	cl, cur, err := app.client()
	if err != nil {
		return err
	}
	hosts, err := cl.Hosts.List(cmd.Context())
	if err != nil {
		return friendly(err)
	}
	if app.isJSON() {
		return printJSON(hosts)
	}
	rows := make([][]string, 0, len(hosts))
	for _, h := range hosts {
		marker := ""
		if h.ID == cur.ActiveHost {
			marker = "*"
		}
		rows = append(rows, []string{
			marker,
			h.ID,
			dash(h.Region),
			h.State,
			fmt.Sprintf("%d/%d", h.Allocated.CPUs, h.Capacity.CPUs),
			fmt.Sprintf("%d/%d", h.Allocated.RamMB, h.Capacity.RamMB),
			gpuCell(h),
			fmt.Sprintf("%d/%d", h.Allocated.VMCount, h.Capacity.VMCount),
			ago(h.LastSeen),
		})
	}
	renderTable([]string{"", "ID", "REGION", "STATE", "CPUS", "RAM MB", "GPUS", "VMS", "LAST SEEN"}, rows)
	return nil
}

// gpuCell renders a host's gpu allocation for the list table, or "-" when the
// host advertises no gpu capacity, so a cpu-only fleet stays readable.
func gpuCell(h fuse.Host) string {
	if h.Capacity.GPUs <= 0 {
		return "-"
	}
	cell := fmt.Sprintf("%d/%d", h.Allocated.GPUs, h.Capacity.GPUs)
	if h.Capacity.GPUKind != "" {
		cell += " (" + h.Capacity.GPUKind + ")"
	}
	return cell
}

// sortedMIGProfiles returns the profile names advertised by a host's capacity
// in stable order, so mig lines render deterministically.
func sortedMIGProfiles(capacity map[string]int) []string {
	profiles := make([]string, 0, len(capacity))
	for profile := range capacity {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}

// formatHostLabels renders a host's declared labels as a sorted, comma-joined
// key=value list. Empty in, empty out (rendered as a dash by the caller).
func formatHostLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key + "=" + labels[key]
	}
	return strings.Join(parts, ", ")
}

// newHostCmd implements `fuse host <id>` (select) plus host subcommands.
func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host [id]",
		Short: "Select a host, or manage hosts (register/get/cordon/...)",
		Long: "with an id, `fuse host <id>` selects the active host that environment and\n" +
			"snapshot commands are scoped to. with no id it shows the current selection.\n" +
			"subcommands (register, get, cordon, uncordon, remove, metrics) take\n" +
			"precedence over selection.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, err := app.cfg.Current(app.ctxName)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if cur.ActiveHost == "" {
					infof("no host selected; run `fuse host <id>` (see `fuse hosts list`)")
					return nil
				}
				infof("active host: %s", cur.ActiveHost)
				return nil
			}
			// validate the host exists before selecting it.
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			id := args[0]
			if _, err := cl.Hosts.Get(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			if err := app.cfg.SetActiveHost(app.ctxName, id); err != nil {
				return err
			}
			if err := app.cfg.Save(); err != nil {
				return err
			}
			successf("selected host %q", id)
			return nil
		},
	}
	cmd.AddCommand(hostManagementCommands()...)
	return cmd
}

func newHostGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show a host's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			h, err := cl.Hosts.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(h)
			}
			renderHostDetail(h)
			return nil
		},
	}
}

func renderHostDetail(h *fuse.Host) {
	rows := [][2]string{
		{"id", h.ID},
		{"url", h.URL},
		{"region", dash(h.Region)},
		{"backend", dash(h.Backend)},
		{"labels", dash(formatHostLabels(h.Labels))},
		{"state", stateStyle(h.State)},
		{"cpus", fmt.Sprintf("%d / %d", h.Allocated.CPUs, h.Capacity.CPUs)},
		{"ram mb", fmt.Sprintf("%d / %d", h.Allocated.RamMB, h.Capacity.RamMB)},
		{"storage gb", fmt.Sprintf("%d / %d", h.Allocated.StorageGB, h.Capacity.StorageGB)},
		{"vms", fmt.Sprintf("%d / %d", h.Allocated.VMCount, h.Capacity.VMCount)},
	}
	if h.Capacity.GPUs > 0 {
		gpuLine := fmt.Sprintf("%d / %d", h.Allocated.GPUs, h.Capacity.GPUs)
		if h.Capacity.GPUKind != "" {
			gpuLine += " (" + h.Capacity.GPUKind + ")"
		}
		rows = append(rows, [2]string{"gpus", gpuLine})
	}
	if len(h.Capacity.MIGProfiles) > 0 {
		profiles := sortedMIGProfiles(h.Capacity.MIGProfiles)
		parts := make([]string, len(profiles))
		for i, profile := range profiles {
			parts[i] = fmt.Sprintf("%s %d / %d", profile,
				h.Allocated.MIGProfiles[profile], h.Capacity.MIGProfiles[profile])
		}
		rows = append(rows, [2]string{"mig", strings.Join(parts, ", ")})
	}
	// per-instance MIG inventory: one row per carved instance with its profile,
	// parent gpu, and whether it is currently bound to a vm. this is the
	// detail the count-map row above summarizes; only present on hosts whose
	// agent reports per-instance inventory.
	if len(h.Capacity.MIGInstances) > 0 {
		bound := make(map[string]bool, len(h.Allocated.MIGInstanceUUIDs))
		for _, u := range h.Allocated.MIGInstanceUUIDs {
			bound[u] = true
		}
		for _, inst := range h.Capacity.MIGInstances {
			status := "free"
			if bound[inst.UUID] {
				status = "in-use"
			}
			val := fmt.Sprintf("%s (%s) parent=%s [%s]",
				inst.Profile, dash(inst.Kind), dash(inst.ParentGPUUUID), status)
			rows = append(rows, [2]string{"mig " + shortUUID(inst.UUID), val})
		}
	}
	rows = append(rows,
		[2]string{"last seen", fmt.Sprintf("%s (%s)", shortTime(h.LastSeen), ago(h.LastSeen))},
		[2]string{"created", shortTime(h.CreatedAt)},
		[2]string{"updated", shortTime(h.UpdatedAt)},
	)
	renderDetail(rows)
	for _, w := range h.Warnings {
		warnf("warning: %s", w)
	}
	warnf("note: last_seen is set at registration and not refreshed; it is not a liveness signal")
}

// newHostActionCmd builds a simple by-id action command (cordon/uncordon).
func newHostActionCmd(name, short string, action func(*fuse.HostsService, context.Context, string) error) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := action(cl.Hosts, cmd.Context(), args[0]); err != nil {
				return friendly(err)
			}
			successf("%s: host %q", name, args[0])
			return nil
		},
	}
}

func newHostRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm", "deregister"},
		Short:   "Deregister a host (refuses if VMs are still assigned)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := confirm(fmt.Sprintf("Deregister host %q?", id))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted (use --yes to skip confirmation)")
				}
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := cl.Hosts.Deregister(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			// clear the selection if we just removed the active host.
			if cur, e := app.cfg.Current(app.ctxName); e == nil && cur.ActiveHost == id {
				_ = app.cfg.SetActiveHost(app.ctxName, "")
				_ = app.cfg.Save()
			}
			successf("removed host %q", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func newHostMetricsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metrics [id]",
		Short: "Show a host's capacity vs allocated (defaults to the active host)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cur, err := app.client()
			if err != nil {
				return err
			}
			id := cur.ActiveHost
			if len(args) == 1 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("no host: pass an id or select one with `fuse host <id>`")
			}
			h, err := cl.Hosts.Get(cmd.Context(), id)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(map[string]any{
					"host_id":   h.ID,
					"state":     h.State,
					"capacity":  h.Capacity,
					"allocated": h.Allocated,
				})
			}
			free := func(alloc, cap int) string { return fmt.Sprintf("%d / %d (%d free)", alloc, cap, cap-alloc) }
			rows := [][2]string{
				{"host", h.ID},
				{"state", stateStyle(h.State)},
				{"cpus", free(h.Allocated.CPUs, h.Capacity.CPUs)},
				{"ram mb", free(h.Allocated.RamMB, h.Capacity.RamMB)},
				{"storage gb", free(h.Allocated.StorageGB, h.Capacity.StorageGB)},
				{"vms", free(h.Allocated.VMCount, h.Capacity.VMCount)},
			}
			// gpu and mig rows appear only when the host advertises the
			// capacity, matching how `fuse host get` renders them.
			if h.Capacity.GPUs > 0 {
				gpuLine := fmt.Sprintf("%d / %d (%d free", h.Allocated.GPUs,
					h.Capacity.GPUs, h.Capacity.GPUs-h.Allocated.GPUs)
				if h.Capacity.GPUKind != "" {
					gpuLine += ", " + h.Capacity.GPUKind
				}
				rows = append(rows, [2]string{"gpus", gpuLine + ")"})
			}
			if len(h.Capacity.MIGProfiles) > 0 {
				profiles := sortedMIGProfiles(h.Capacity.MIGProfiles)
				parts := make([]string, len(profiles))
				for i, profile := range profiles {
					parts[i] = fmt.Sprintf("%s %s", profile,
						free(h.Allocated.MIGProfiles[profile], h.Capacity.MIGProfiles[profile]))
				}
				rows = append(rows, [2]string{"mig", strings.Join(parts, ", ")})
			}
			renderDetail(rows)
			return nil
		},
	}
}

func newHostRegisterCmd() *cobra.Command {
	var (
		hostURL     string
		region      string
		token       string
		backend     string
		cpus        int
		ramMB       int
		storageGB   int
		maxVMs      int
		gpus        int
		gpuKind     string
		migProfiles []string
		labels      []string
		noVerify    bool
	)
	cmd := &cobra.Command{
		Use:   "register <id>",
		Short: "Register a new compute host (operator tier)",
		Long: "register adds a compute host to the orchestrator's scheduler. the id is a\n" +
			"free-form operator-supplied string and is the host's primary key, so use a\n" +
			"readable one (e.g. prod-east-1). pass details via flags, or omit --url to\n" +
			"fill them in interactively.\n\n" +
			"--cpus/--ram-mb/--storage-gb/--gpus/--gpu-kind are optional: omit one (or\n" +
			"leave it at 0/empty) and the orchestrator probes the host agent for the real\n" +
			"value instead of trusting a guess. pass one explicitly to override the probe\n" +
			"(e.g. a deliberate overcommit or carve-out); a declared value above what was\n" +
			"probed still registers, with a warning. gpus are probed on qemu hosts;\n" +
			"a host with no gpus is fine. --max-vms is a scheduling policy, not a hardware\n" +
			"fact, so it is always required.\n\n" +
			"--label declares operator-chosen key=value pairs a Fusefile's placement.labels\n" +
			"block can select on (e.g. --label disk=nvme). labels are never probed: they\n" +
			"are a claim about the box, like --gpu-kind.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if hostURL == "" {
				if !isInteractive() {
					return fmt.Errorf("host url is required: pass --url (or run interactively)")
				}
				cpusS, ramS, storS, vmsS, gpusS := strconv.Itoa(cpus), strconv.Itoa(ramMB), strconv.Itoa(storageGB), strconv.Itoa(maxVMs), strconv.Itoa(gpus)
				err := runForm(huh.NewGroup(
					huh.NewInput().Title("Host URL").Description("agent base url, e.g. http://10.0.0.5:9000").Value(&hostURL),
					huh.NewInput().Title("Host agent token").Description("the agent's FC_AGENT_TOKEN (not the orchestrator token)").EchoMode(huh.EchoModePassword).Value(&token),
					huh.NewInput().Title("Region").Value(&region),
					huh.NewInput().Title("CPUs (capacity)").Description("0 = probe from host agent").Value(&cpusS).Validate(validateInt),
					huh.NewInput().Title("RAM MB (capacity)").Description("0 = probe from host agent").Value(&ramS).Validate(validateInt),
					huh.NewInput().Title("Storage GB (capacity)").Description("0 = probe from host agent").Value(&storS).Validate(validateInt),
					huh.NewInput().Title("Max VMs (capacity)").Description("required; scheduling policy, not probed").Value(&vmsS).Validate(validateInt),
					huh.NewInput().Title("GPUs (capacity)").Description("0 = probe from host agent").Value(&gpusS).Validate(validateInt),
					huh.NewInput().Title("GPU kind (capacity)").Description("empty = probe from host agent").Value(&gpuKind),
				))
				if err != nil {
					return err
				}
				cpus, _ = strconv.Atoi(cpusS)
				ramMB, _ = strconv.Atoi(ramS)
				storageGB, _ = strconv.Atoi(storS)
				maxVMs, _ = strconv.Atoi(vmsS)
				gpus, _ = strconv.Atoi(gpusS)
			}
			if hostURL == "" {
				return fmt.Errorf("host url is required: pass --url")
			}
			migMap, err := parseMIGProfiles(migProfiles)
			if err != nil {
				return err
			}
			labelMap, err := parseHostLabels(labels)
			if err != nil {
				return err
			}
			// The firecracker host agent authenticates the orchestrator with
			// its FC_AGENT_TOKEN. An empty token is almost always a mistake
			// that only surfaces at the first `environment create` as an
			// opaque 401, so require it here unless the operator opts out for
			// a genuinely unauthenticated dev agent.
			effectiveBackend := backend
			if effectiveBackend == "" {
				effectiveBackend = "firecracker"
			}
			if effectiveBackend == "firecracker" && token == "" && !noVerify {
				return fmt.Errorf("host agent token required for the firecracker backend: pass --token (the agent's FC_AGENT_TOKEN), or --no-verify for an unauthenticated dev agent")
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			h, err := cl.Hosts.Register(cmd.Context(), fuse.RegisterHostRequest{
				ID:      id,
				URL:     hostURL,
				Token:   token,
				Region:  region,
				Backend: backend,
				Labels:  labelMap,
				Capacity: fuse.HostCapacity{
					CPUs:        cpus,
					RamMB:       ramMB,
					StorageGB:   storageGB,
					VMCount:     maxVMs,
					GPUs:        gpus,
					GPUKind:     gpuKind,
					MIGProfiles: migMap,
				},
			})
			if err != nil {
				return friendly(err)
			}
			successf("registered host %q", id)
			if app.isJSON() {
				return printJSON(h)
			}
			renderHostDetail(h)
			return nil
		},
	}
	cmd.Flags().StringVar(&hostURL, "url", "", "host agent base url (required)")
	cmd.Flags().StringVar(&region, "region", "", "region label")
	cmd.Flags().StringVar(&token, "token", "", "host agent token the orchestrator uses to call the host (its FC_AGENT_TOKEN)")
	cmd.Flags().StringVar(&backend, "backend", "", "virtualization backend: firecracker (default) or qemu")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "allow an empty --token (unauthenticated dev agent)")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "cpu capacity override (0 = probe from host agent)")
	cmd.Flags().IntVar(&ramMB, "ram-mb", 0, "ram capacity in MB override (0 = probe from host agent)")
	cmd.Flags().IntVar(&storageGB, "storage-gb", 0, "storage capacity in GB override (0 = probe from host agent)")
	cmd.Flags().IntVar(&maxVMs, "max-vms", 0, "max vm count (required; not probed, it's a scheduling policy)")
	cmd.Flags().IntVar(&gpus, "gpus", 0, "gpu capacity override (0 = probe from host agent; requires --backend qemu)")
	cmd.Flags().StringVar(&gpuKind, "gpu-kind", "", "gpu model label override, e.g. a100 (empty = probe from host agent)")
	cmd.Flags().StringArrayVar(&migProfiles, "mig-profile", nil,
		"MIG instance capacity as profile=count (e.g. 1g.10gb=4); repeatable, requires --backend qemu")
	cmd.Flags().StringArrayVar(&labels, "label", nil,
		"placement label as key=value (e.g. disk=nvme); repeatable, declared not probed")
	return cmd
}

// parseHostLabels turns repeated --label key=value flags into the label map
// the register wire expects. Nil in, nil out.
func parseHostLabels(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("invalid --label %q: expected key=value (e.g. disk=nvme)", entry)
		}
		out[key] = value
	}
	return out, nil
}

// parseMIGProfiles turns repeated --mig-profile profile=count flags into the
// capacity map the register wire expects. Nil in, nil out.
func parseMIGProfiles(entries []string) (map[string]int, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(entries))
	for _, entry := range entries {
		profile, countS, ok := strings.Cut(entry, "=")
		if !ok || profile == "" {
			return nil, fmt.Errorf("invalid --mig-profile %q: expected profile=count (e.g. 1g.10gb=4)", entry)
		}
		count, err := strconv.Atoi(countS)
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("invalid --mig-profile %q: count must be a positive number", entry)
		}
		out[strings.ToLower(profile)] = count
	}
	return out, nil
}

func validateInt(s string) error {
	if s == "" {
		return nil
	}
	if _, err := strconv.Atoi(s); err != nil {
		return fmt.Errorf("must be a number")
	}
	return nil
}
