/*
Copyright 2022 The KCP Authors.

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

package catalogentry

import (
	"context"
	"time"

	"errors"
	"fmt"
	"strings"

	kcpclienthelper "github.com/kcp-dev/apimachinery/pkg/client"
	catalogv1alpha1 "github.com/kcp-dev/catalog/api/v1alpha1"
	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	pluginhelpers "github.com/kcp-dev/kcp/pkg/cliplugins/helpers"
	"github.com/kcp-dev/logicalcluster/v2"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kcp-dev/kcp/pkg/cliplugins/base"
	"k8s.io/client-go/rest"
)

// BindOptions contains the options for creating APIBindings for CE
type BindOptions struct {
	*base.Options
	// CatalogEntryRef is the argument accepted by the command. It contains the
	// reference to where CatalogEntry exists. For ex: <absolute_ref_to_workspace>:<catalogEntry>.
	CatalogEntryRef string
	// BindWaitTimeout is how long to wait for the apibindings to be created and successful.
	BindWaitTimeout time.Duration
}

// NewBindOptions returns new BindOptions.
func NewBindOptions(streams genericclioptions.IOStreams) *BindOptions {
	return &BindOptions{
		Options:         base.NewOptions(streams),
		BindWaitTimeout: 30 * time.Second,
	}
}

// BindFlags binds fields to cmd's flagset.
func (b *BindOptions) BindFlags(cmd *cobra.Command) {
	b.Options.BindFlags(cmd)
	cmd.Flags().DurationVar(&b.BindWaitTimeout, "timeout", b.BindWaitTimeout, "Duration to wait for the bindings to be created and bound successfully.")
}

// Complete ensures all fields are initialized.
func (b *BindOptions) Complete(args []string) error {
	if err := b.Options.Complete(); err != nil {
		return err
	}

	if len(args) > 0 {
		b.CatalogEntryRef = args[0]
	}
	return nil
}

// Validate validates the BindOptions are complete and usable.
func (b *BindOptions) Validate() error {
	if b.CatalogEntryRef == "" {
		return errors.New("`root:ws:catalogentry_object` reference to bind is required as an argument")
	}

	if !strings.HasPrefix(b.CatalogEntryRef, "root") || !logicalcluster.New(b.CatalogEntryRef).IsValid() {
		return fmt.Errorf("fully qualified reference to workspace where catalog entry exists is required. The format is `root:<ws>:<catalogentry>`")
	}

	return b.Options.Validate()
}

// Run creates an apibinding for the user.
func (b *BindOptions) Run(ctx context.Context) error {
	config, err := b.ClientConfig.ClientConfig()
	if err != nil {
		return err
	}

	baseURL, currentClusterName, err := pluginhelpers.ParseClusterURL(config.Host)
	if err != nil {
		return err
	}

	// get the base config, which is needed for creation of clients.
	baseConfig, err := ctrlcfg.GetConfigWithContext("base")
	if err != nil {
		return fmt.Errorf("unable to get base config %v", err)
	}

	// get the entry referenced in the command to which the user wants to bind.
	path, entryName := logicalcluster.New(b.CatalogEntryRef).Split()
	cfg := rest.CopyConfig(config)
	cfg.Host = baseURL.String()
	client, err := newClient(cfg, path)
	if err != nil {
		return err
	}

	entry := catalogv1alpha1.CatalogEntry{}
	err = client.Get(ctx, types.NamespacedName{Name: entryName}, &entry)
	if err != nil {
		return fmt.Errorf("cannot find the catalog entry %q referenced in the command in the workspace %q", entryName, path)
	}

	kcpClient, err := newClient(baseConfig, currentClusterName)
	if err != nil {
		return err
	}

	allErrors := []error{}

	apiBindings := []apisv1alpha1.APIBinding{}
	for _, ref := range entry.Spec.Exports {
		// check if ref is valid. Skip if invalid by logging error.
		if ref.Workspace.Path == "" || ref.Workspace.ExportName == "" {
			if _, err := fmt.Fprintf(b.Out, "invalid reference %q/%q", ref.Workspace.Path, ref.Workspace.ExportName); err != nil {
				allErrors = append(allErrors, err)
			}
			continue
		}

		apiBinding := &apisv1alpha1.APIBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: ref.Workspace.ExportName,
			},
			Spec: apisv1alpha1.APIBindingSpec{
				Reference: ref,
			},
		}

		apiBindings = append(apiBindings, *apiBinding)
	}

	// Create bindings to the target workspace
	for _, binding := range apiBindings {
		err := kcpClient.Create(ctx, &binding)
		if err != nil {
			// If an APIBinding already exists, intentionally not updating it since we would not like reset AcceptablePermissionClaims.
			if _, err := fmt.Fprintf(b.Out, "Failed to create API binding %s: %s", binding.Name, err.Error()); err != nil {
				allErrors = append(allErrors, err)
			}
			continue
		}
	}

	if err := wait.PollImmediate(time.Millisecond*500, b.BindWaitTimeout, func() (done bool, err error) {
		availableBindings := []apisv1alpha1.APIBinding{}
		for _, binding := range apiBindings {
			createdBinding := apisv1alpha1.APIBinding{}
			err = kcpClient.Get(ctx, types.NamespacedName{Name: binding.Name}, &createdBinding)
			if err != nil {
				return false, err
			}
			availableBindings = append(availableBindings, createdBinding)
		}
		return bindReady(availableBindings), nil
	}); err != nil {
		return fmt.Errorf("bindings for catalog entry %s could not be created successfully: %v", entryName, err)
	}

	if _, err := fmt.Fprintf(b.Out, "%s created and bound to catalog entry.\n", entryName); err != nil {
		allErrors = append(allErrors, err)
	}
	return utilerrors.NewAggregate(allErrors)
}

func bindReady(bindings []apisv1alpha1.APIBinding) bool {
	for _, binding := range bindings {
		if binding.Status.Phase != apisv1alpha1.APIBindingPhaseBound {
			return false
		}
	}
	return true
}

func newClient(cfg *rest.Config, clusterName logicalcluster.Name) (client.Client, error) {
	scheme := runtime.NewScheme()
	err := apisv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = catalogv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	return client.New(kcpclienthelper.SetCluster(rest.CopyConfig(cfg), clusterName), client.Options{
		Scheme: scheme,
	})
}
