package main

import (
	"fmt"
	"os"
	"time"

	"github.com/flant/kubedog/pkg/kube"
	"github.com/flant/kubedog/pkg/tracker"
	"github.com/flant/kubedog/pkg/trackers/follow"
	"github.com/flant/kubedog/pkg/trackers/rollout"
	"github.com/spf13/cobra"
)

func main() {
	err := kube.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to initialize kube: %s", err)
		os.Exit(1)
	}

	var namespace string
	var timeoutSeconds uint

	makeTrackerOptions := func() tracker.Options {
		return tracker.Options{Timeout: time.Second * time.Duration(timeoutSeconds)}
	}

	rootCmd := &cobra.Command{Use: "kubedog"}
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "kubernetes namespace")
	rootCmd.PersistentFlags().UintVarP(&timeoutSeconds, "timeout", "t", 300, "watch timeout in seconds")

	followCmd := &cobra.Command{Use: "follow"}
	rootCmd.AddCommand(followCmd)

	followCmd.AddCommand(&cobra.Command{
		Use:   "job NAME",
		Short: "Follow Job",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := follow.TrackJob(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error following Job `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})
	followCmd.AddCommand(&cobra.Command{
		Use:   "deployment NAME",
		Short: "Follow Deployment",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := follow.TrackDeployment(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error following Deployment `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})
	followCmd.AddCommand(&cobra.Command{
		Use:   "pod NAME",
		Short: "Follow Pod",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := follow.TrackPod(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error following Pod `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})

	rolloutCmd := &cobra.Command{Use: "rollout"}
	rootCmd.AddCommand(rolloutCmd)
	trackCmd := &cobra.Command{Use: "track"}
	rolloutCmd.AddCommand(trackCmd)

	trackCmd.AddCommand(&cobra.Command{
		Use:   "job NAME",
		Short: "Track Job till job is done",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := rollout.TrackJobTillDone(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error tracking Job `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})

	trackCmd.AddCommand(&cobra.Command{
		Use:   "deployment NAME",
		Short: "Track Deployment till ready",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := rollout.TrackDeploymentTillReady(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error tracking Deployment `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})

	trackCmd.AddCommand(&cobra.Command{
		Use:   "pod NAME",
		Short: "Track Pod till ready",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			err := rollout.TrackPodTillReady(name, namespace, kube.Kubernetes, makeTrackerOptions())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error tracking Pod `%s` in namespace `%s`: %s\n", name, namespace, err)
				os.Exit(1)
			}
		},
	})

	err = rootCmd.Execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
