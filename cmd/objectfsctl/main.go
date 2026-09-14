/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	pb "github.com/gke-labs/in-cluster-storage/pkg/api/objectfs/v1alpha1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type options struct {
	serverAddr string
}

func dialServer(addr string) (pb.ObjectFSControllerClient, func() error, error) {
	if addr == "" {
		return nil, nil, fmt.Errorf("--server is required")
	}

	target := addr
	if strings.HasPrefix(target, "tcp://") {
		target = strings.TrimPrefix(target, "tcp://")
	}

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to server at %s: %w", addr, err)
	}

	client := pb.NewObjectFSControllerClient(conn)
	return client, conn.Close, nil
}

func NewRootCommand() *cobra.Command {
	opts := &options{}

	rootCmd := &cobra.Command{
		Use:           "objectfsctl",
		Aliases:       []string{"objectfs"},
		Short:         "objectfs administrative CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.serverAddr, "server", "", "ObjectFS server address (e.g. localhost:50051 or unix:///path/to/socket)")

	rootCmd.AddCommand(newBlobsCommand(opts))

	return rootCmd
}

func newBlobsCommand(opts *options) *cobra.Command {
	blobsCmd := &cobra.Command{
		Use:     "blobs",
		Aliases: []string{"blob"},
		Short:   "Manage underlying objects/blobs in the store",
	}

	blobsCmd.AddCommand(newBlobsListCommand(opts))
	blobsCmd.AddCommand(newBlobsGetCommand(opts))

	return blobsCmd
}

type blobsListOptions struct {
	*options
	from   string
	limit  int32
	prefix string
}

func newBlobsListCommand(opts *options) *cobra.Command {
	listOpts := &blobsListOptions{options: opts}
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all blobs in the store",
		RunE: func(cmd *cobra.Command, args []string) error {
			if listOpts.serverAddr == "" {
				return fmt.Errorf("--server is required")
			}

			client, closeConn, err := dialServer(listOpts.serverAddr)
			if err != nil {
				return err
			}
			defer closeConn()

			fromSHA := listOpts.from
			remainingLimit := listOpts.limit
			out := cmd.OutOrStdout()

			for {
				reqLimit := remainingLimit
				resp, err := client.ListBlobs(cmd.Context(), &pb.ListBlobsRequest{
					FromSha:   fromSHA,
					Limit:     reqLimit,
					ShaPrefix: listOpts.prefix,
				})
				if err != nil {
					return fmt.Errorf("failed to list blobs: %w", err)
				}

				for _, sha := range resp.GetSha256() {
					fmt.Fprintln(out, sha)
					fromSHA = sha
					if remainingLimit > 0 {
						remainingLimit--
						if remainingLimit == 0 {
							return nil
						}
					}
				}

				if resp.GetEndOfData() || len(resp.GetSha256()) == 0 {
					break
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listOpts.from, "from", "", "List blobs starting after this SHA256")
	cmd.Flags().Int32Var(&listOpts.limit, "limit", 0, "Maximum number of blobs to list (0 = no limit)")
	cmd.Flags().StringVar(&listOpts.prefix, "prefix", "", "Filter blobs starting with this SHA256 prefix")
	return cmd
}

type blobsGetOptions struct {
	*options
	offset int64
	limit  int64
}

func newBlobsGetCommand(opts *options) *cobra.Command {
	getOpts := &blobsGetOptions{options: opts}
	cmd := &cobra.Command{
		Use:   "get <sha256>",
		Short: "Print the contents of a blob to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if getOpts.serverAddr == "" {
				return fmt.Errorf("--server is required")
			}

			sha := args[0]
			client, closeConn, err := dialServer(getOpts.serverAddr)
			if err != nil {
				return err
			}
			defer closeConn()

			stream, err := client.GetBlob(cmd.Context(), &pb.GetBlobRequest{
				Sha256: sha,
				Offset: getOpts.offset,
				Limit:  getOpts.limit,
			})
			if err != nil {
				return fmt.Errorf("failed to get blob: %w", err)
			}

			out := cmd.OutOrStdout()
			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("failed receiving blob data: %w", err)
				}
				if len(chunk.GetData()) > 0 {
					if _, err := out.Write(chunk.GetData()); err != nil {
						return fmt.Errorf("failed writing to output: %w", err)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&getOpts.offset, "offset", 0, "Offset in bytes to start reading from")
	cmd.Flags().Int64Var(&getOpts.limit, "limit", 0, "Maximum number of bytes to read (0 = no limit)")
	return cmd
}

func main() {
	rootCmd := NewRootCommand()
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
