package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/internal/fusefile"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newBuildCmd() *cobra.Command {
	var (
		file              string
		name              string
		secrets           []string
		secretsFile       string
		allowEmptySecrets bool
		keep              bool
		plan              bool
		noCache           bool
		detach            bool
	)
	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Run a Fusefile's setup phase once and snapshot the result",
		Long: "build has the orchestrator boot a throwaway environment, run the\n" +
			"Fusefile's setup phase in it, snapshot the resulting disk, and destroy the\n" +
			"environment. it follows the build and prints the snapshot id, which\n" +
			"`fuse up --from-build <id>` boots directly so the setup work does not\n" +
			"rerun on every boot. the build runs on the orchestrator, so it finishes\n" +
			"and cleans up its builder even if this command is stopped; --detach\n" +
			"prints the build id and exits at once.\n\n" +
			"the artifact is stored on the host that produced it. there is no object\n" +
			"storage, but it is not stuck there either: a host that needs it fetches\n" +
			"it from a host that has it. firecracker hosts only, since qemu hosts\n" +
			"cannot snapshot.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := findFusefilePath(file, args)
			if err != nil {
				return err
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			f, err := fusefile.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := fusefile.ResolveFiles(f, filepath.Dir(path)); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if noCache {
				f.Cache.Enabled = false
			}
			c, err := fusefile.Compile(f)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if c.BuildScript == "" {
				return fmt.Errorf("%s: no setup phase to build", path)
			}

			// build is where the setup phase actually runs, so the layer plan
			// describes exactly this command's work. derive it before booting
			// the builder: a bad `inputs` entry should fail here rather than
			// after a vm is up and holding host capacity.
			lp, err := planSetupLayers(path, f, plan, "")
			if err != nil {
				return err
			}
			if plan {
				return renderLayerPlan(lp)
			}

			secretMap, err := loadSecretsFile(secretsFile)
			if err != nil {
				return err
			}
			if secretMap == nil {
				secretMap = map[string]string{}
			}
			flagSecrets, err := parseKeyVals(secrets)
			if err != nil {
				return err
			}
			// --secret flags override --secrets-file entries on key collision,
			// matching `fuse up`.
			for k, v := range flagSecrets {
				secretMap[k] = v
			}
			if missing := missingSecrets(c.RequiredSecrets, secretMap, allowEmptySecrets); len(missing) > 0 {
				return fmt.Errorf("missing required secrets: %s (pass --allow-empty-secrets to accept empty values)", strings.Join(missing, ", "))
			}

			if name == "" {
				name = defaultTaskID(path)
			}

			cl, cur, err := app.client()
			if err != nil {
				return err
			}

			// a `from:` image is converted first, and the layer keys root at
			// the digest that comes back, so the plan is derived again with it.
			imageDigest := ""
			if c.Spec.From != "" {
				img, err := convertImage(cmd.Context(), cl, fuse.ConvertImageRequest{Ref: c.Spec.From, HostID: c.Spec.HostID, Region: c.Spec.Region, Labels: c.Spec.Labels})
				if err != nil {
					return err
				}
				imageDigest = img.Digest
				if lp != nil {
					if lp, err = planSetupLayers(path, f, false, imageDigest); err != nil {
						return err
					}
				}
			}

			// the layer keys are derived here because only this machine can
			// read the files a step's `inputs` name. the orchestrator looks
			// them up and runs the rest.
			setup := fusefile.NewSetupPlan(f)
			arch := ""
			if lp != nil && lp.CacheEnabled {
				arch = targetArch(cmd.Context(), cl, cur.ActiveHost)
				if arch == "" {
					// no safe answer, so no lookup. filing a layer under an arch
					// it was not built on would serve a later build a rootfs it
					// cannot boot, which is far worse than rebuilding.
					warnf("cannot determine the target host architecture; building without the layer cache")
					lp.CacheEnabled = false
				}
			}
			req := buildRequest(name, c, secretMap, setup, lp, arch, keep)
			req.Spec.ImageDigest = imageDigest

			b, err := cl.Builds.Create(cmd.Context(), req)
			if err != nil {
				return friendly(err)
			}
			if detach {
				if app.isJSON() {
					return printJSON(b)
				}
				successf("build %s submitted", b.ID)
				fmt.Println(b.ID)
				return nil
			}
			infof("build %s submitted; it keeps running on the orchestrator if this command stops", b.ID)

			b, err = followBuild(cmd.Context(), cl, b)
			if err != nil {
				return err
			}
			if keep && b.BuilderID != "" {
				warnf("keeping builder environment %s", b.BuilderID)
			}
			if b.State != fuse.BuildSucceeded {
				if b.Output != "" {
					fmt.Fprint(os.Stderr, b.Output)
				}
				return fmt.Errorf("build %s failed: %s", b.ID, b.Error)
			}

			if app.isJSON() {
				snap, err := cl.Snapshots.Get(cmd.Context(), b.SnapshotID)
				if err != nil {
					return friendly(err)
				}
				return printJSON(snap)
			}
			successf("built %s", b.SnapshotID)
			// the id goes to stdout so `fuse up --from-build "$(fuse build)"`
			// works; all other chatter is on stderr.
			fmt.Println(b.SnapshotID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the Fusefile (default: ./Fusefile, or the positional path)")
	cmd.Flags().StringVar(&name, "name", "", "name for the build artifact (default: the Fusefile's parent directory name)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "secret as key=value (repeatable, overrides --secrets-file)")
	cmd.Flags().StringVar(&secretsFile, "secrets-file", "", "path to a file of KEY=VALUE secret lines")
	cmd.Flags().BoolVar(&allowEmptySecrets, "allow-empty-secrets", false, "treat an empty value as satisfying a required secret")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the builder environment running instead of destroying it")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the derived setup layer cache plan and exit without building anything")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "ignore the Fusefile's cache block and run every setup step")
	cmd.Flags().BoolVar(&detach, "detach", false, "submit the build, print its id and exit without following it")
	return cmd
}

// buildRequest is the build the orchestrator runs for c: the setup plan with
// each step's layer key from lp. a plan with the cache off sends no keys, so
// the whole setup runs as one exec, byte for byte the build script.
func buildRequest(name string, c *fusefile.Compiled, secretMap map[string]string, setup fusefile.SetupPlan, lp *layerPlan, arch string, keep bool) fuse.CreateBuildRequest {
	req := fuse.CreateBuildRequest{
		Name: name,
		Spec: fuse.Spec{
			CPUs:      c.Spec.CPUs,
			RamMB:     c.Spec.RamMB,
			StorageGB: c.Spec.StorageGB,
			Region:    c.Spec.Region,
			Image:     c.Spec.Image,
		},
		ManifestInline: base64.StdEncoding.EncodeToString(c.ManifestJSON),
		Secrets:        secretMap,
		Keep:           keep,
		Setup:          fuse.BuildSetup{Prelude: setup.Prelude, Files: setup.Files},
	}
	cached := lp != nil && lp.CacheEnabled
	if cached {
		req.Arch = arch
	}
	for i, script := range setup.Steps {
		step := fuse.BuildStep{Script: script}
		if cached && i < len(lp.Steps) && lp.Steps[i].Cacheable {
			step.Key = lp.Steps[i].Key
		}
		req.Setup.Steps = append(req.Setup.Steps, step)
	}
	return req
}

// followBuild waits for b to finish, reporting each step it moves to. the
// build keeps running if ctx ends first.
func followBuild(ctx context.Context, cl *fuse.Client, b *fuse.Build) (*fuse.Build, error) {
	step, builder := "", ""
	for {
		if b.BuilderID != "" && b.BuilderID != builder {
			builder = b.BuilderID
			if b.CacheHits > 0 {
				successf("reusing %d cached layer(s) from %s", b.CacheHits, b.SeedSnapshotID)
			}
			successf("building in environment %s", builder)
		}
		if b.Step != step {
			step = b.Step
			infof("%s", step)
		}
		if b.Done() {
			return b, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(buildPollInterval):
		}
		next, err := cl.Builds.Get(ctx, b.ID, fuse.GetBuildOptions{})
		if err != nil {
			if ctx.Err() != nil {
				warnf("stopped following build %s; it keeps running on the orchestrator", b.ID)
			}
			return nil, friendly(err)
		}
		b = next
	}
}

// buildPollInterval is how often a followed build is read. a short poll
// rather than a long ?wait=, which answers only when the build finishes, so
// each step shows as the build reaches it.
var buildPollInterval = time.Second
