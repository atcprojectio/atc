package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

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
		cfg, err := loadCLIConfig()
		if err != nil {
			return err
		}
		// Leader commands inspect the HA locks in Consul. Without this, a missing or
		// non-HA config made "leader status" report STANDBY/<none> without asking Consul.
		cfg.HA.Enabled = true

		t := atc.NewCLI(cfg)

		ctx := context.Background()
		fmt.Printf("LOCAL NODE: %s\n\n", t.Cfg.Name)
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

		cfg, err := loadCLIConfig()
		if err != nil {
			return err
		}
		// Leader commands inspect the HA locks in Consul. Without this, a missing or
		// non-HA config made "leader status" report STANDBY/<none> without asking Consul.
		cfg.HA.Enabled = true

		t := atc.NewCLI(cfg)

		ctx := context.Background()
		err = t.ForceUnlock(ctx, forceModule)
		if err != nil {
			return fmt.Errorf("failed to force unlock module %q: %w", forceModule, err)
		}

		fmt.Printf("Lock for '%s' has been successfully forced unlocked.\n", forceModule)
		return nil
	},
}

func init() {
	forceUnlockCmd.Flags().StringVar(&forceModule, "module", "", "Module to force unlock ('forwarder' or 'redirector')")
	_ = forceUnlockCmd.MarkFlagRequired("module")

	leaderCmd.AddCommand(leaderStatusCmd)
	leaderCmd.AddCommand(forceUnlockCmd)
	rootCmd.AddCommand(leaderCmd)
}
