package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

var leaderCmd = &cobra.Command{
	Use:   "leader",
	Short: "Leader election and status commands",
	Long:  "Leader election and status commands",
}

var leaderStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Display cluster leadership status",
	Long:  "Display cluster leadership status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		t, err := atc.New(cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize ATC: %w", err)
		}

		ctx := context.Background()
		fmt.Printf("LOCAL NODE: %s\n\n", cfg.Name)
		fmt.Printf("%-12s %-10s %-30s %-15s %s\n", "MODULE", "STATUS", "LOCK KEY", "LEADER NODE", "SESSION ID")

		for _, module := range []string{"forwarder", "redirector"} {
			details, err := t.GetLeaderDetails(ctx, module)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error fetching details for %s: %v\n", module, err)
				continue
			}

			status := "STANDBY"
			if details.IsLeader {
				status = "LEADER"
			}

			session := details.SessionID
			if len(session) > 8 {
				session = session[:8] + "..."
			}

			leaderNode := details.LeaderNode
			if leaderNode == "" {
				leaderNode = "<none>"
				status = "STANDBY"
			}

			fmt.Printf("%-12s %-10s %-30s %-15s %s\n", module, status, details.LockKey, leaderNode, session)
		}
		return nil
	},
}

var forceModule string

var forceUnlockCmd = &cobra.Command{
	Use:   "force-unlock",
	Short: "Force unlock leadership locks for a module",
	Long:  "Force unlock leadership locks for a module",
	RunE: func(cmd *cobra.Command, args []string) error {
		if forceModule != "forwarder" && forceModule != "redirector" {
			return fmt.Errorf("invalid module %q (must be 'forwarder' or 'redirector')", forceModule)
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		t, err := atc.New(cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize ATC: %w", err)
		}

		ctx := context.Background()
		err = t.ForceUnlock(ctx, forceModule)
		if err != nil {
			return fmt.Errorf("failed to force unlock module %q: %w", forceModule, err)
		}

		fmt.Printf("Lock for '%s' has been successfully forced unlocked.\n", forceModule)
		return nil
	},
}

func loadConfig() (atc.Config, error) {
	var cfg atc.Config
	configFile := viper.GetString("config")
	if configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return cfg, fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}
	}

	if err := viper.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	if cfg.ConsulAddr == "" {
		cfg.ConsulAddr = viper.GetString("consul_addr")
	}
	if cfg.ConsulToken == "" {
		cfg.ConsulToken = viper.GetString("consul_token")
	}
	if cfg.ConsulDC == "" {
		cfg.ConsulDC = viper.GetString("consul_dc")
	}
	if cfg.ConsulNamespace == "" {
		cfg.ConsulNamespace = viper.GetString("consul_namespace")
	}
	if cfg.Name == "" {
		cfg.Name = viper.GetString("name")
	}

	return cfg, nil
}

func init() {
	forceUnlockCmd.Flags().StringVar(&forceModule, "module", "", "Module to force unlock ('forwarder' or 'redirector')")
	_ = forceUnlockCmd.MarkFlagRequired("module")

	leaderCmd.AddCommand(leaderStatusCmd)
	leaderCmd.AddCommand(forceUnlockCmd)
	rootCmd.AddCommand(leaderCmd)
}
