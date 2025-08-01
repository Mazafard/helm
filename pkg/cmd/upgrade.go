/*
Copyright The Helm Authors.

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

package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli/output"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/cmd/require"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
)

const upgradeDesc = `
This command upgrades a release to a new version of a chart.

The upgrade arguments must be a release and chart. The chart
argument can be either: a chart reference('example/mariadb'), a path to a chart directory,
a packaged chart, or a fully qualified URL. For chart references, the latest
version will be specified unless the '--version' flag is set.

To override values in a chart, use either the '--values' flag and pass in a file
or use the '--set' flag and pass configuration from the command line, to force string
values, use '--set-string'. You can use '--set-file' to set individual
values from a file when the value itself is too long for the command line
or is dynamically generated. You can also use '--set-json' to set json values
(scalars/objects/arrays) from the command line. Additionally, you can use '--set-json' and passing json object as a string.

You can specify the '--values'/'-f' flag multiple times. The priority will be given to the
last (right-most) file specified. For example, if both myvalues.yaml and override.yaml
contained a key called 'Test', the value set in override.yaml would take precedence:

    $ helm upgrade -f myvalues.yaml -f override.yaml redis ./redis

You can specify the '--set' flag multiple times. The priority will be given to the
last (right-most) set specified. For example, if both 'bar' and 'newbar' values are
set for a key called 'foo', the 'newbar' value would take precedence:

    $ helm upgrade --set foo=bar --set foo=newbar redis ./redis

You can update the values for an existing release with this command as well via the
'--reuse-values' flag. The 'RELEASE' and 'CHART' arguments should be set to the original
parameters, and existing values will be merged with any values set via '--values'/'-f'
or '--set' flags. Priority is given to new values.

    $ helm upgrade --reuse-values --set foo=bar --set foo=newbar redis ./redis

The --dry-run flag will output all generated chart manifests, including Secrets
which can contain sensitive values. To hide Kubernetes Secrets use the
--hide-secret flag. Please carefully consider how and when these flags are used.
`

func newUpgradeCmd(cfg *action.Configuration, out io.Writer) *cobra.Command {
	client := action.NewUpgrade(cfg)
	valueOpts := &values.Options{}
	var outfmt output.Format
	var createNamespace bool

	cmd := &cobra.Command{
		Use:   "upgrade [RELEASE] [CHART]",
		Short: "upgrade a release",
		Long:  upgradeDesc,
		Args:  require.ExactArgs(2),
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return compListReleases(toComplete, args, cfg)
			}
			if len(args) == 1 {
				return compListCharts(toComplete, true)
			}
			return noMoreArgsComp()
		},
		RunE: func(_ *cobra.Command, args []string) error {
			client.Namespace = settings.Namespace()

			registryClient, err := newRegistryClient(client.CertFile, client.KeyFile, client.CaFile,
				client.InsecureSkipTLSverify, client.PlainHTTP, client.Username, client.Password)
			if err != nil {
				return fmt.Errorf("missing registry client: %w", err)
			}
			client.SetRegistryClient(registryClient)

			// This is for the case where "" is specifically passed in as a
			// value. When there is no value passed in NoOptDefVal will be used
			// and it is set to client. See addInstallFlags.
			if client.DryRunOption == "" {
				client.DryRunOption = "none"
			}
			// Fixes #7002 - Support reading values from STDIN for `upgrade` command
			// Must load values AFTER determining if we have to call install so that values loaded from stdin are not read twice
			if client.Install {
				// If a release does not exist, install it.
				histClient := action.NewHistory(cfg)
				histClient.Max = 1
				versions, err := histClient.Run(args[0])
				if err == driver.ErrReleaseNotFound || isReleaseUninstalled(versions) {
					// Only print this to stdout for table output
					if outfmt == output.Table {
						_, _ = fmt.Fprintf(out, "Release %q does not exist. Installing it now.\n", args[0])
					}
					instClient := action.NewInstall(cfg)
					instClient.CreateNamespace = createNamespace
					instClient.ChartPathOptions = client.ChartPathOptions
					instClient.Force = client.Force
					instClient.DryRun = client.DryRun
					instClient.DryRunOption = client.DryRunOption
					instClient.DisableHooks = client.DisableHooks
					instClient.SkipCRDs = client.SkipCRDs
					instClient.Timeout = client.Timeout
					instClient.WaitStrategy = client.WaitStrategy
					instClient.WaitForJobs = client.WaitForJobs
					instClient.Devel = client.Devel
					instClient.Namespace = client.Namespace
					instClient.Atomic = client.Atomic
					instClient.PostRenderer = client.PostRenderer
					instClient.DisableOpenAPIValidation = client.DisableOpenAPIValidation
					instClient.SubNotes = client.SubNotes
					instClient.HideNotes = client.HideNotes
					instClient.SkipSchemaValidation = client.SkipSchemaValidation
					instClient.Description = client.Description
					instClient.DependencyUpdate = client.DependencyUpdate
					instClient.Labels = client.Labels
					instClient.EnableDNS = client.EnableDNS
					instClient.HideSecret = client.HideSecret
					instClient.TakeOwnership = client.TakeOwnership

					if isReleaseUninstalled(versions) {
						instClient.Replace = true
					}

					rel, err := runInstall(args, instClient, valueOpts, out)
					if err != nil {
						return err
					}
					return outfmt.Write(out, &statusPrinter{
						release:      rel,
						debug:        settings.Debug,
						showMetadata: false,
						hideNotes:    instClient.HideNotes,
						noColor:      settings.NoColor,
					})
				} else if err != nil {
					return err
				}
			}

			if client.Version == "" && client.Devel {
				slog.Debug("setting version to >0.0.0-0")
				client.Version = ">0.0.0-0"
			}

			chartPath, err := client.LocateChart(args[1], settings)
			if err != nil {
				return err
			}
			// Validate dry-run flag value is one of the allowed values
			if err := validateDryRunOptionFlag(client.DryRunOption); err != nil {
				return err
			}

			p := getter.All(settings)
			vals, err := valueOpts.MergeValues(p)
			if err != nil {
				return err
			}

			// Check chart dependencies to make sure all are present in /charts
			ch, err := loader.Load(chartPath)
			if err != nil {
				return err
			}
			if req := ch.Metadata.Dependencies; req != nil {
				if err := action.CheckDependencies(ch, req); err != nil {
					err = fmt.Errorf("an error occurred while checking for chart dependencies. You may need to run `helm dependency build` to fetch missing dependencies: %w", err)
					if client.DependencyUpdate {
						man := &downloader.Manager{
							Out:              out,
							ChartPath:        chartPath,
							Keyring:          client.Keyring,
							SkipUpdate:       false,
							Getters:          p,
							RepositoryConfig: settings.RepositoryConfig,
							RepositoryCache:  settings.RepositoryCache,
							Debug:            settings.Debug,
						}
						if err := man.Update(); err != nil {
							return err
						}
						// Reload the chart with the updated Chart.lock file.
						if ch, err = loader.Load(chartPath); err != nil {
							return fmt.Errorf("failed reloading chart after repo update: %w", err)
						}
					} else {
						return err
					}
				}
			}

			if ch.Metadata.Deprecated {
				slog.Warn("this chart is deprecated")
			}

			// Create context and prepare the handle of SIGTERM
			ctx := context.Background()
			ctx, cancel := context.WithCancel(ctx)

			// Set up channel on which to send signal notifications.
			// We must use a buffered channel or risk missing the signal
			// if we're not ready to receive when the signal is sent.
			cSignal := make(chan os.Signal, 2)
			signal.Notify(cSignal, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-cSignal
				_, _ = fmt.Fprintf(out, "Release %s has been cancelled.\n", args[0])
				cancel()
			}()

			rel, err := client.RunWithContext(ctx, args[0], ch, vals)
			if err != nil {
				return fmt.Errorf("UPGRADE FAILED: %w", err)
			}

			if outfmt == output.Table {
				_, _ = fmt.Fprintf(out, "Release %q has been upgraded. Happy Helming!\n", args[0])
			}

			return outfmt.Write(out, &statusPrinter{
				release:      rel,
				debug:        settings.Debug,
				showMetadata: false,
				hideNotes:    client.HideNotes,
				noColor:      settings.NoColor,
			})
		},
	}

	f := cmd.Flags()
	f.BoolVar(&createNamespace, "create-namespace", false, "if --install is set, create the release namespace if not present")
	f.BoolVarP(&client.Install, "install", "i", false, "if a release by this name doesn't already exist, run an install")
	f.BoolVar(&client.Devel, "devel", false, "use development versions, too. Equivalent to version '>0.0.0-0'. If --version is set, this is ignored")
	f.StringVar(&client.DryRunOption, "dry-run", "", "simulate an install. If --dry-run is set with no option being specified or as '--dry-run=client', it will not attempt cluster connections. Setting '--dry-run=server' allows attempting cluster connections.")
	f.BoolVar(&client.HideSecret, "hide-secret", false, "hide Kubernetes Secrets when also using the --dry-run flag")
	f.Lookup("dry-run").NoOptDefVal = "client"
	f.BoolVar(&client.Force, "force", false, "force resource updates through a replacement strategy")
	f.BoolVar(&client.DisableHooks, "no-hooks", false, "disable pre/post upgrade hooks")
	f.BoolVar(&client.DisableOpenAPIValidation, "disable-openapi-validation", false, "if set, the upgrade process will not validate rendered templates against the Kubernetes OpenAPI Schema")
	f.BoolVar(&client.SkipCRDs, "skip-crds", false, "if set, no CRDs will be installed when an upgrade is performed with install flag enabled. By default, CRDs are installed if not already present, when an upgrade is performed with install flag enabled")
	f.DurationVar(&client.Timeout, "timeout", 300*time.Second, "time to wait for any individual Kubernetes operation (like Jobs for hooks)")
	f.BoolVar(&client.ResetValues, "reset-values", false, "when upgrading, reset the values to the ones built into the chart")
	f.BoolVar(&client.ReuseValues, "reuse-values", false, "when upgrading, reuse the last release's values and merge in any overrides from the command line via --set and -f. If '--reset-values' is specified, this is ignored")
	f.BoolVar(&client.ResetThenReuseValues, "reset-then-reuse-values", false, "when upgrading, reset the values to the ones built into the chart, apply the last release's values and merge in any overrides from the command line via --set and -f. If '--reset-values' or '--reuse-values' is specified, this is ignored")
	f.BoolVar(&client.WaitForJobs, "wait-for-jobs", false, "if set and --wait enabled, will wait until all Jobs have been completed before marking the release as successful. It will wait for as long as --timeout")
	f.BoolVar(&client.Atomic, "atomic", false, "if set, upgrade process rolls back changes made in case of failed upgrade. The --wait flag will be set automatically to \"watcher\" if --atomic is used")
	f.IntVar(&client.MaxHistory, "history-max", settings.MaxHistory, "limit the maximum number of revisions saved per release. Use 0 for no limit")
	f.BoolVar(&client.CleanupOnFail, "cleanup-on-fail", false, "allow deletion of new resources created in this upgrade when upgrade fails")
	f.BoolVar(&client.SubNotes, "render-subchart-notes", false, "if set, render subchart notes along with the parent")
	f.BoolVar(&client.HideNotes, "hide-notes", false, "if set, do not show notes in upgrade output. Does not affect presence in chart metadata")
	f.BoolVar(&client.SkipSchemaValidation, "skip-schema-validation", false, "if set, disables JSON schema validation")
	f.StringToStringVarP(&client.Labels, "labels", "l", nil, "Labels that would be added to release metadata. Should be separated by comma. Original release labels will be merged with upgrade labels. You can unset label using null.")
	f.StringVar(&client.Description, "description", "", "add a custom description")
	f.BoolVar(&client.DependencyUpdate, "dependency-update", false, "update dependencies if they are missing before installing the chart")
	f.BoolVar(&client.EnableDNS, "enable-dns", false, "enable DNS lookups when rendering templates")
	f.BoolVar(&client.TakeOwnership, "take-ownership", false, "if set, upgrade will ignore the check for helm annotations and take ownership of the existing resources")
	addChartPathOptionsFlags(f, &client.ChartPathOptions)
	addValueOptionsFlags(f, valueOpts)
	bindOutputFlag(cmd, &outfmt)
	bindPostRenderFlag(cmd, &client.PostRenderer)
	AddWaitFlag(cmd, &client.WaitStrategy)

	err := cmd.RegisterFlagCompletionFunc("version", func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 2 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return compVersionFlag(args[1], toComplete)
	})
	if err != nil {
		log.Fatal(err)
	}

	return cmd
}

func isReleaseUninstalled(versions []*release.Release) bool {
	return len(versions) > 0 && versions[len(versions)-1].Info.Status == release.StatusUninstalled
}
