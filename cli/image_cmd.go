package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// imagePollInterval is how often a converting image is read. a bake takes
// minutes, so there is no point asking more often.
var imagePollInterval = 2 * time.Second

// newImageCmd implements `fuse image`: convert an oci image (what a
// Fusefile's `from:` does before `up`) and read one back by digest.
func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Aliases: []string{"images"},
		Short:   "Convert oci images into bootable rootfs images",
	}
	cmd.AddCommand(newImageConvertCmd(), newImageGetCmd())
	return cmd
}

func newImageConvertCmd() *cobra.Command {
	var (
		host   string
		region string
		labels []string
		noWait bool
	)
	cmd := &cobra.Command{
		Use:   "convert <ref>",
		Short: "Convert an oci image (e.g. ubuntu:24.04) to a rootfs on a firecracker host",
		Long: "convert resolves the ref for a firecracker host's arch and bakes it into a\n" +
			"rootfs there, then waits until it is ready unless --no-wait is set. the\n" +
			"digest it prints is what a create names as spec.image_digest; a Fusefile's\n" +
			"`from:` does all of this as part of `fuse up`. public registries only,\n" +
			"and only apt and dnf based images convert.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			labelMap, err := parseKeyVals(labels)
			if err != nil {
				return err
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			req := fuse.ConvertImageRequest{Ref: args[0], HostID: host, Region: region, Labels: labelMap}
			var img *fuse.Image
			if noWait {
				img, err = cl.Images.Convert(cmd.Context(), req)
				if err != nil {
					return friendly(err)
				}
			} else if img, err = convertImage(cmd.Context(), cl, req); err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(img)
			}
			renderImageDetail(img)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "convert on this firecracker host")
	cmd.Flags().StringVar(&region, "region", "", "convert on a firecracker host in this region")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "convert on a host with this label, as key=value (repeatable)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "start the conversion and exit without waiting for it")
	return cmd
}

func newImageGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <digest>",
		Short: "Show an image's conversion state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			img, err := cl.Images.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(img)
			}
			renderImageDetail(img)
			return nil
		},
	}
}

func renderImageDetail(img *fuse.Image) {
	rows := [][2]string{
		{"digest", img.Digest},
		{"ref", img.Ref},
		{"manifest", dash(img.ManifestDigest)},
		{"host", dash(img.HostID)},
		{"state", stateStyle(img.State)},
	}
	if img.Error != "" {
		rows = append(rows, [2]string{"error", img.Error})
	}
	renderDetail(rows)
}

// convertImage starts converting req.Ref and waits until it is ready. a
// failed conversion is an error carrying the host's reason, so the caller
// never goes on to create from an image that is not there.
func convertImage(ctx context.Context, cl *fuse.Client, req fuse.ConvertImageRequest) (*fuse.Image, error) {
	img, err := cl.Images.Convert(ctx, req)
	if err != nil {
		return nil, friendly(err)
	}
	if img.State == fuse.ImageConverting {
		infof("converting %s on host %s (image %s)", req.Ref, img.HostID, img.Digest)
	}
	for img.State == fuse.ImageConverting {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("stopped waiting for image %s; it keeps converting on host %s (fuse image get %s)", img.Digest, img.HostID, img.Digest)
		case <-time.After(imagePollInterval):
		}
		if img, err = cl.Images.Get(ctx, img.Digest); err != nil {
			return nil, friendly(err)
		}
	}
	if img.State != fuse.ImageReady {
		return nil, fmt.Errorf("image %s (%s) is %s on host %s: %s", req.Ref, img.Digest, img.State, img.HostID, dash(img.Error))
	}
	successf("image %s ready on host %s", req.Ref, img.HostID)
	return img, nil
}
