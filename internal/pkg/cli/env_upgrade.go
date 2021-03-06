// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"

	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy"
	"github.com/aws/copilot-cli/internal/pkg/describe"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

const (
	envUpgradeAppPrompt = "In which application is your environment?"

	envUpgradeEnvPrompt = "Which environment do you want to upgrade?"
	envUpgradeEnvHelp   = `Upgrades the AWS CloudFormation template for your environment
to support the latest Copilot features.`
)

// envUpgradeVars holds flag values.
type envUpgradeVars struct {
	appName string // Required. Name of the application.
	name    string // Required. Name of the environment.
	all     bool   // True means all environments should be upgraded.
}

// envUpgradeOpts represents the env upgrade command and holds the necessary data
// and clients to execute the command.
type envUpgradeOpts struct {
	envUpgradeVars

	store environmentStore
	sel   appEnvSelector

	// Constructors for clients that can be initialized only at runtime.
	// These functions are overriden in tests to provide mocks.
	newEnvVersionGetter func(app, env string) (versionGetter, error)
}

func newEnvUpgradeOpts(vars envUpgradeVars) (*envUpgradeOpts, error) {
	store, err := config.NewStore()
	if err != nil {
		return nil, fmt.Errorf("connect to config store: %v", err)
	}
	return &envUpgradeOpts{
		envUpgradeVars: vars,

		store: store,
		sel:   selector.NewSelect(prompt.New(), store),

		newEnvVersionGetter: func(app, env string) (versionGetter, error) {
			d, err := describe.NewEnvDescriber(describe.NewEnvDescriberConfig{
				App:         app,
				Env:         env,
				ConfigStore: store,
			})
			if err != nil {
				return nil, fmt.Errorf("new env describer for environment %s in app %s: %v", env, app, err)
			}
			return d, nil
		},
	}, nil
}

// Validate returns an error if the values passed by flags are invalid.
func (o *envUpgradeOpts) Validate() error {
	if o.all && o.name != "" {
		return fmt.Errorf("cannot specify both --%s and --%s flags", allFlag, nameFlag)
	}
	if o.all {
		return nil
	}
	if o.name != "" {
		if _, err := o.store.GetEnvironment(o.appName, o.name); err != nil {
			var errEnvDoesNotExist *config.ErrNoSuchEnvironment
			if errors.As(err, &errEnvDoesNotExist) {
				return err
			}
			return fmt.Errorf("get environment %s configuration from application %s: %v", o.name, o.appName, err)
		}
	}
	return nil
}

// Ask prompts for any required flags that are not set by the user.
func (o *envUpgradeOpts) Ask() error {
	if o.appName == "" {
		app, err := o.sel.Application(envUpgradeAppPrompt, "")
		if err != nil {
			return fmt.Errorf("select application: %v", err)
		}
		o.appName = app
	}
	if !o.all && o.name == "" {
		env, err := o.sel.Environment(envUpgradeEnvPrompt, envUpgradeEnvHelp, o.appName)
		if err != nil {
			return fmt.Errorf("select environment: %v", err)
		}
		o.name = env
	}
	return nil
}

// Execute updates the cloudformation stack of an environment to the latest version.
// If the environment stack is busy updating, it spins and waits until the stack can be updated.
func (o *envUpgradeOpts) Execute() error {
	envs, err := o.listEnvsToUpgrade()
	if err != nil {
		return err
	}
	for _, env := range envs {
		if err := o.upgrade(env); err != nil {
			return err
		}
	}
	return nil
}

func (o *envUpgradeOpts) listEnvsToUpgrade() ([]string, error) {
	if !o.all {
		return []string{o.name}, nil
	}

	envs, err := o.store.ListEnvironments(o.appName)
	if err != nil {
		return nil, fmt.Errorf("list environments in app %s: %v", o.appName, err)
	}
	var names []string
	for _, env := range envs {
		names = append(names, env.Name)
	}
	return names, nil
}

func (o *envUpgradeOpts) upgrade(env string) error {
	yes, err := o.shouldUpgrade(env)
	if err != nil {
		return err
	}
	if !yes {
		return nil
	}
	// If the environment's version is a legacy version,
	// and the template was generated by customizing the VPC,
	// and the customization configuration is not stored in SSM (see #1433),
	// then we cannot upgrade the environment without re-asking for the VPC configuration.
	return nil
}

func (o *envUpgradeOpts) shouldUpgrade(env string) (bool, error) {
	envTpl, err := o.newEnvVersionGetter(o.appName, env)
	if err != nil {
		return false, err
	}
	version, err := envTpl.Version()
	if err != nil {
		return false, fmt.Errorf("get template version of environment %s in app %s: %v", env, o.appName, err)
	}

	diff := semver.Compare(version, deploy.LatestEnvTemplateVersion)
	if diff < 0 {
		// Newer version available.
		return true, nil
	}

	msg := fmt.Sprintf("Environment %s is already on the latest version %s, skip upgrade.", env, deploy.LatestEnvTemplateVersion)
	if diff > 0 {
		// It's possible that a teammate used a different version of the CLI to upgrade the environment
		// to a newer version. And the current user is on an older version of the CLI.
		// In this situation we notify them they should update the CLI.
		msg = fmt.Sprintf(`Skip upgrading environment %s to version %s since it's on version %s. 
Are you using the latest version of AWS Copilot?`, env, deploy.LatestEnvTemplateVersion, version)
	}
	log.Debugln(msg)
	return false, nil
}

// buildEnvUpgradeCmd builds the command to update environment(s) to the latest version of
// the environment template.
func buildEnvUpgradeCmd() *cobra.Command {
	vars := envUpgradeVars{}
	cmd := &cobra.Command{
		Use:    "upgrade",
		Short:  "Upgrades the template of an environment to the latest version.",
		Hidden: true,
		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newEnvUpgradeOpts(vars)
			if err != nil {
				return err
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			if err := opts.Ask(); err != nil {
				return err
			}
			return opts.Execute()
		}),
	}
	cmd.Flags().StringVarP(&vars.name, nameFlag, nameFlagShort, "", envFlagDescription)
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().BoolVar(&vars.all, allFlag, false, upgradeAllEnvsDescription)
	return cmd
}
