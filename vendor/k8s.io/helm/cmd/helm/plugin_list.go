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

package main

import (
	"fmt"
	"io"

	"github.com/gosuri/uitable"
	"github.com/spf13/cobra"

	"k8s.io/helm/pkg/helm/helmpath"
)

type pluginListOptions struct {
	home helmpath.Home
}

func newPluginListCmd(out io.Writer) *cobra.Command {
	o := &pluginListOptions{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list installed Helm plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			o.home = settings.Home
			return o.run(out)
		},
	}
	return cmd
}

func (o *pluginListOptions) run(out io.Writer) error {
	debug("pluginDirs: %s", settings.PluginDirs())
	plugins, err := findPlugins(settings.PluginDirs())
	if err != nil {
		return err
	}

	table := uitable.New()
	table.AddRow("NAME", "VERSION", "DESCRIPTION")
	for _, p := range plugins {
		table.AddRow(p.Metadata.Name, p.Metadata.Version, p.Metadata.Description)
	}
	fmt.Fprintln(out, table)
	return nil
}
