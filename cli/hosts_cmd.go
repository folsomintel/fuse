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
	return cmd
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
			fmt.Sprintf("%d/%d", h.Allocated.VMCount, h.Capacity.VMCount),
			ago(h.LastSeen),
		})
	}
	renderTable([]string{"", "ID", "REGION", "STATE", "CPUS", "RAM MB", "VMS", "LAST SEEN"}, rows)
	return nil
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
	cmd.AddCommand(
		newHostRegisterCmd(),
		newHostGetCmd(),
		newHostActionCmd("cordon", "Mark a host unschedulable", (*fuse.HostsService).Cordon),
		newHostActionCmd("uncordon", "Mark a host schedulable again", (*fuse.HostsService).Uncordon),
		newHostRemoveCmd(),
		newHostMetricsCmd(),
	)
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
		profiles := make([]string, 0, len(h.Capacity.MIGProfiles))
		for profile := range h.Capacity.MIGProfiles {
			profiles = append(profiles, profile)
		}
		sort.Strings(profiles)
		parts := make([]string, len(profiles))
		for i, profile := range profiles {
			parts[i] = fmt.Sprintf("%s %d / %d", profile,
				h.Allocated.MIGProfiles[profile], h.Capacity.MIGProfiles[profile])
		}
		rows = append(rows, [2]string{"mig", strings.Join(parts, ", ")})
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
			renderDetail([][2]string{
				{"host", h.ID},
				{"state", stateStyle(h.State)},
				{"cpus", free(h.Allocated.CPUs, h.Capacity.CPUs)},
				{"ram mb", free(h.Allocated.RamMB, h.Capacity.RamMB)},
				{"storage gb", free(h.Allocated.StorageGB, h.Capacity.StorageGB)},
				{"vms", free(h.Allocated.VMCount, h.Capacity.VMCount)},
			})
			return nil
		},
	}
}

func newHostRegisterCmd() *cobra.Command {
	var (
		hostURL   string
		region    string
		token     string
		backend   string
		cpus        int
		ramMB       int
		storageGB   int
		maxVMs      int
		gpus        int
		gpuKind     string
		migProfiles []string
	)
	cmd := &cobra.Command{
		Use:   "register <id>",
		Short: "Register a new compute host (operator tier)",
		Long: "register adds a compute host to the orchestrator's scheduler. the id is a\n" +
			"free-form operator-supplied string and is the host's primary key, so use a\n" +
			"readable one (e.g. prod-east-1). pass details via flags, or omit --url to\n" +
			"fill them in interactively.\n\n" +
			"--cpus/--ram-mb/--storage-gb are optional: omit one (or leave it at 0) and\n" +
			"the orchestrator probes the host agent for the real value instead of trusting\n" +
			"a guess. pass one explicitly to override the probe (e.g. a deliberate\n" +
			"overcommit or carve-out); a declared value above what was probed still\n" +
			"registers, with a warning. --max-vms is a scheduling policy, not a hardware\n" +
			"fact, so it is always required.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if hostURL == "" {
				if !isInteractive() {
					return fmt.Errorf("host url is required: pass --url (or run interactively)")
				}
				cpusS, ramS, storS, vmsS := strconv.Itoa(cpus), strconv.Itoa(ramMB), strconv.Itoa(storageGB), strconv.Itoa(maxVMs)
				err := runForm(huh.NewGroup(
					huh.NewInput().Title("Host URL").Description("agent base url, e.g. http://10.0.0.5:9000").Value(&hostURL),
					huh.NewInput().Title("Region").Value(&region),
					huh.NewInput().Title("CPUs (capacity)").Description("0 = probe from host agent").Value(&cpusS).Validate(validateInt),
					huh.NewInput().Title("RAM MB (capacity)").Description("0 = probe from host agent").Value(&ramS).Validate(validateInt),
					huh.NewInput().Title("Storage GB (capacity)").Description("0 = probe from host agent").Value(&storS).Validate(validateInt),
					huh.NewInput().Title("Max VMs (capacity)").Description("required; scheduling policy, not probed").Value(&vmsS).Validate(validateInt),
				))
				if err != nil {
					return err
				}
				cpus, _ = strconv.Atoi(cpusS)
				ramMB, _ = strconv.Atoi(ramS)
				storageGB, _ = strconv.Atoi(storS)
				maxVMs, _ = strconv.Atoi(vmsS)
			}
			if hostURL == "" {
				return fmt.Errorf("host url is required: pass --url")
			}
			migMap, err := parseMIGProfiles(migProfiles)
			if err != nil {
				return err
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
	cmd.Flags().StringVar(&token, "token", "", "token the orchestrator uses to call the host")
	cmd.Flags().StringVar(&backend, "backend", "", "virtualization backend: firecracker (default) or qemu")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "cpu capacity override (0 = probe from host agent)")
	cmd.Flags().IntVar(&ramMB, "ram-mb", 0, "ram capacity in MB override (0 = probe from host agent)")
	cmd.Flags().IntVar(&storageGB, "storage-gb", 0, "storage capacity in GB override (0 = probe from host agent)")
	cmd.Flags().IntVar(&maxVMs, "max-vms", 0, "max vm count (required; not probed, it's a scheduling policy)")
	cmd.Flags().IntVar(&gpus, "gpus", 0, "gpu device count (requires --backend qemu)")
	cmd.Flags().StringVar(&gpuKind, "gpu-kind", "", "gpu model label (e.g. a100)")
	cmd.Flags().StringArrayVar(&migProfiles, "mig-profile", nil,
		"MIG instance capacity as profile=count (e.g. 1g.10gb=4); repeatable, requires --backend qemu")
	return cmd
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
