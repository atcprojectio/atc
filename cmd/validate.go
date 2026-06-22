package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate and lint the configuration file.",
	Long:  "Parse the strategies config file and check it against semantic validation and schema constraints.",
	Run: func(cmd *cobra.Command, args []string) {
		var cfg atc.Config
		configFile := viper.GetString("config")
		if configFile == "" {
			fmt.Fprintln(os.Stderr, "Error: --config flag is required for validation")
			os.Exit(1)
		}

		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading config file %s: %v\n", configFile, err)
			os.Exit(1)
		}

		if err := viper.Unmarshal(&cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error unmarshaling configuration: %v\n", err)
			os.Exit(1)
		}

		errs := atc.ValidateConfig(cfg)
		if len(errs) > 0 {
			fmt.Fprintf(os.Stderr, "Configuration validation failed for %s with %d error(s):\n", configFile, len(errs))
			for _, err := range errs {
				fmt.Fprintf(os.Stderr, "  - %v\n", err)
			}
			os.Exit(1)
		}

		fmt.Printf("Configuration %s is valid!\n", configFile)
	},
}

func init() {
	rootCmd.AddCommand(validateCmd)

	validateCmd.Flags().String("config", "", "Path to ATC configuration file to validate.")
	_ = viper.BindPFlag("config", validateCmd.Flags().Lookup("config"))
	_ = validateCmd.MarkFlagRequired("config")
}
