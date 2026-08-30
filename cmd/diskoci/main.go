// Command diskoci pushes and pulls VM disk images to/from OCI registries.
package main

import (
	"fmt"
	"os"
	"strings"

	diskoci "github.com/devcell-sh/go-diskoci"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "diskoci",
		Short:         "Store VM disk images in OCI registries",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newPushCmd(), newPullCmd())
	return root
}

// authOptions builds credential options from flags, falling back to
// DISKOCI_USERNAME/DISKOCI_PASSWORD, then the default keychain.
func authOptions(username, password string) []diskoci.Option {
	if username == "" {
		username = os.Getenv("DISKOCI_USERNAME")
	}
	if password == "" {
		password = os.Getenv("DISKOCI_PASSWORD")
	}
	if username != "" || password != "" {
		return []diskoci.Option{diskoci.WithCredentials(username, password)}
	}
	return nil
}

func newPushCmd() *cobra.Command {
	var (
		chunkSize    int64
		sourceFormat string
		annotations  []string
		username     string
		password     string
	)
	cmd := &cobra.Command{
		Use:   "push <image-path> <name[:tag]>",
		Short: "Push a disk image to a registry (tag defaults to :latest)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			diskPath, ref := args[0], args[1]

			opts := authOptions(username, password)
			opts = append(opts, diskoci.WithChunkSize(chunkSize))
			if sourceFormat != "" {
				opts = append(opts, diskoci.WithSourceFormat(sourceFormat))
			}
			if len(annotations) > 0 {
				parsed := make(map[string]string, len(annotations))
				for _, a := range annotations {
					k, v, ok := strings.Cut(a, "=")
					if !ok || k == "" {
						return fmt.Errorf("invalid --annotation %q, want key=value", a)
					}
					parsed[k] = v
				}
				opts = append(opts, diskoci.WithAnnotations(parsed))
			}

			digest, err := diskoci.Push(cmd.Context(), ref, diskPath, opts...)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", digest)
			return nil
		},
	}
	cmd.Flags().Int64Var(&chunkSize, "chunk-size", diskoci.DefaultChunkSize, "uncompressed layer size in bytes")
	cmd.Flags().StringVar(&sourceFormat, "source-format", "", "input format (qcow2|raw|vhdx); default: auto-detect")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "manifest annotation key=value (repeatable)")
	cmd.Flags().StringVarP(&username, "username", "u", "", "registry username (env: DISKOCI_USERNAME)")
	cmd.Flags().StringVarP(&password, "password", "p", "", "registry password (env: DISKOCI_PASSWORD)")
	return cmd
}

// defaultPullPath derives the destination filename from the ref's image name
// and the requested output format: ghcr.io/org/win11:abc → win11.qcow2.
func defaultPullPath(ref, outputFormat string) string {
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name, _, _ = strings.Cut(name, ":")
	ext := outputFormat
	if ext == "" {
		ext = "qcow2" // the stored wire format
	}
	return name + "." + ext
}

func newPullCmd() *cobra.Command {
	var (
		outputFormat string
		username     string
		password     string
	)
	cmd := &cobra.Command{
		Use:   "pull <name[:tag]> [<image-path>]",
		Short: "Pull a disk image from a registry (default path: <imageName>.<ext>)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			var destPath string
			if len(args) == 2 {
				destPath = args[1]
			} else {
				destPath = defaultPullPath(ref, outputFormat)
			}

			opts := authOptions(username, password)
			if outputFormat != "" {
				opts = append(opts, diskoci.WithOutputFormat(outputFormat))
			}

			if err := diskoci.Pull(cmd.Context(), ref, destPath, opts...); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", destPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&outputFormat, "output-format", "", "output format (qcow2|raw|vhdx); default: qcow2 as stored")
	cmd.Flags().StringVarP(&username, "username", "u", "", "registry username (env: DISKOCI_USERNAME)")
	cmd.Flags().StringVarP(&password, "password", "p", "", "registry password (env: DISKOCI_PASSWORD)")
	return cmd
}
