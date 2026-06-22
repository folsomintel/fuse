package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	fuse "github.com/andrewn6/fuse/sdks/go"
)

func newEnvironmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "environment",
		Aliases: []string{"environments", "env", "envs"},
		Short:   "Manage environments (VMs) on the active host",
	}
	cmd.AddCommand(
		newEnvListCmd(),
		newEnvGetCmd(),
		newEnvCreateCmd(),
		newEnvDestroyCmd(),
		newEnvDrainCmd(),
		newEnvRotateTokenCmd(),
		newEnvWatchCmd(),
	)
	return cmd
}

func newEnvListCmd() *cobra.Command {
	var (
		taskID string
		state  string
		hostID string
		all    bool
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List environments (scoped to the active host by default)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, cur, err := app.client()
			if err != nil {
				return err
			}
			// default scope: active host, unless --all or an explicit --host-id.
			if hostID == "" && !all {
				hostID = cur.ActiveHost
			}
			envs, err := cl.Environments.List(cmd.Context(), fuse.ListEnvironmentsOptions{
				TaskID: taskID,
				State:  state,
				HostID: hostID,
			})
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(envs)
			}
			rows := make([][]string, 0, len(envs))
			for _, e := range envs {
				rows = append(rows, []string{
					e.ID, e.State, dash(e.TaskID), dash(e.HostID), dash(e.URL), shortTime(e.CreatedAt),
				})
			}
			renderTable([]string{"ID", "STATE", "TASK ID", "HOST", "URL", "CREATED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&taskID, "task-id", "", "filter by task id")
	cmd.Flags().StringVar(&state, "state", "", "filter by state (provisioning|running|draining|destroying|destroyed|failed)")
	cmd.Flags().StringVar(&hostID, "host-id", "", "filter by host id (default: active host)")
	cmd.Flags().BoolVar(&all, "all", false, "list across all hosts (ignore the active host)")
	return cmd
}

func newEnvGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show an environment's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			e, err := cl.Environments.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(e)
			}
			renderEnvDetail(e)
			return nil
		},
	}
}

func renderEnvDetail(e *fuse.EnvironmentInfo) {
	pairs := [][2]string{
		{"id", e.ID},
		{"state", stateStyle(e.State)},
		{"task id", dash(e.TaskID)},
		{"host id", dash(e.HostID)},
		{"url", dash(e.URL)},
		{"cpus", strconv.Itoa(int(e.Spec.CPUs))},
		{"ram mb", strconv.Itoa(int(e.Spec.RamMB))},
		{"storage gb", strconv.Itoa(int(e.Spec.StorageGB))},
		{"region", dash(e.Spec.Region)},
		{"max runtime s", strconv.FormatInt(e.Spec.MaxRuntimeSeconds, 10)},
		{"created", shortTime(e.CreatedAt)},
		{"updated", shortTime(e.UpdatedAt)},
	}
	if e.Error != "" {
		pairs = append(pairs, [2]string{"error", styleBad.Render(e.Error)})
	}
	renderDetail(pairs)
}

func newEnvCreateCmd() *cobra.Command {
	var (
		taskID        string
		cpus          int32
		ramMB         int32
		storageGB     int32
		region        string
		maxRuntime    int64
		manifest      string
		startupScript string
		gatewayURL    string
		gatewayToken  string
		secrets       []string
		follow        bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an environment",
		Long: "create provisions a new environment. it requires a task id and a spec.\n" +
			"with no --task-id and a tty, the required fields are collected interactively.\n" +
			"note: scheduling is orchestrator-wide; the active host does not pin placement\n" +
			"(use spec.region to influence it).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" && isInteractive() {
				cpusS, ramS, storS, runS := i32s(cpus), i32s(ramMB), i32s(storageGB), strconv.FormatInt(maxRuntime, 10)
				err := runForm(huh.NewGroup(
					huh.NewInput().Title("Task ID").Description("required").Value(&taskID).Validate(notEmpty),
					huh.NewInput().Title("CPUs").Value(&cpusS).Validate(validateInt),
					huh.NewInput().Title("RAM MB").Value(&ramS).Validate(validateInt),
					huh.NewInput().Title("Storage GB").Value(&storS).Validate(validateInt),
					huh.NewInput().Title("Region").Value(&region),
					huh.NewInput().Title("Max runtime (seconds, 0 = unlimited)").Value(&runS).Validate(validateInt),
				))
				if err != nil {
					return err
				}
				cpus, ramMB, storageGB = atoi32(cpusS), atoi32(ramS), atoi32(storS)
				maxRuntime, _ = strconv.ParseInt(runS, 10, 64)
			}
			if taskID == "" {
				return fmt.Errorf("task id is required: pass --task-id (or run interactively)")
			}
			secretMap, err := parseKeyVals(secrets)
			if err != nil {
				return err
			}
			manifestVal, err := maybeFile(manifest)
			if err != nil {
				return err
			}
			startupVal, err := maybeFile(startupScript)
			if err != nil {
				return err
			}
			cl, cur, err := app.client()
			if err != nil {
				return err
			}
			if cur.ActiveHost != "" {
				infof("note: create is orchestrator-scheduled; the active host %q does not pin placement", cur.ActiveHost)
			}
			e, err := cl.Environments.Create(cmd.Context(), fuse.CreateRequest{
				TaskID: taskID,
				Spec: fuse.Spec{
					CPUs:              cpus,
					RamMB:             ramMB,
					StorageGB:         storageGB,
					Region:            region,
					MaxRuntimeSeconds: maxRuntime,
				},
				ManifestInline: manifestVal,
				Secrets:        secretMap,
				StartupScript:  startupVal,
				GatewayURL:     gatewayURL,
				GatewayToken:   gatewayToken,
			})
			if err != nil {
				return friendly(err)
			}
			successf("creating environment %s (task %s)", e.ID, e.TaskID)
			if follow {
				return streamEnvironment(cmd.Context(), cl, e.ID)
			}
			if app.isJSON() {
				return printJSON(e)
			}
			renderEnvDetail(e)
			infof("watch progress with: fuse environment watch %s", e.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&taskID, "task-id", "", "task id (required)")
	cmd.Flags().Int32Var(&cpus, "cpus", 0, "cpus")
	cmd.Flags().Int32Var(&ramMB, "ram-mb", 0, "ram in MB")
	cmd.Flags().Int32Var(&storageGB, "storage-gb", 0, "storage in GB")
	cmd.Flags().StringVar(&region, "region", "", "region")
	cmd.Flags().Int64Var(&maxRuntime, "max-runtime", 0, "max runtime in seconds (0 = unlimited)")
	cmd.Flags().StringVar(&manifest, "manifest", "", "inline manifest, or @path to read from a file")
	cmd.Flags().StringVar(&startupScript, "startup-script", "", "startup script, or @path to read from a file")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "gateway url")
	cmd.Flags().StringVar(&gatewayToken, "gateway-token", "", "gateway token")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable)")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream provisioning events until terminal state")
	return cmd
}

func newEnvDestroyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "destroy <id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Destroy an environment",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := confirm(fmt.Sprintf("Destroy environment %q?", id))
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
			if err := cl.Environments.Destroy(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			successf("destroying environment %q", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func newEnvDrainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drain <id>",
		Short: "Gracefully drain an environment (running -> draining)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			e, err := cl.Environments.Drain(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			successf("draining environment %q", args[0])
			infof("drain is phase-1 only; it does not auto-destroy. run `fuse environment destroy %s` to remove it", args[0])
			if app.isJSON() {
				return printJSON(e)
			}
			renderEnvDetail(e)
			return nil
		},
	}
}

func newEnvRotateTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rotate-token <id>",
		Aliases: []string{"rotate"},
		Short:   "Re-issue the guest agent credentials for an environment",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := cl.Environments.RotateToken(cmd.Context(), args[0]); err != nil {
				return friendly(err)
			}
			successf("rotated token for environment %q", args[0])
			return nil
		},
	}
}

func newEnvWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "watch <id>",
		Aliases: []string{"follow"},
		Short:   "Stream an environment's state transitions until terminal",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			return streamEnvironment(cmd.Context(), cl, args[0])
		},
	}
}

// --- small input helpers ---

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("required")
	}
	return nil
}

func i32s(v int32) string { return strconv.Itoa(int(v)) }

func atoi32(s string) int32 {
	n, _ := strconv.Atoi(s)
	return int32(n)
}

// parseKeyVals turns ["k=v", ...] into a map.
func parseKeyVals(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(items))
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid key=value %q", item)
		}
		m[k] = v
	}
	return m, nil
}

// maybeFile returns the contents of a file when s starts with '@', otherwise s
// verbatim.
func maybeFile(s string) (string, error) {
	if !strings.HasPrefix(s, "@") {
		return s, nil
	}
	data, err := os.ReadFile(s[1:])
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s[1:], err)
	}
	return string(data), nil
}
