package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc"
)

// TestThirdPartyActionsArePinned: workflows that handle release secrets must not run
// third-party actions by mutable tag. Pin them to a full commit SHA with a version comment
// ("uses: owner/repo@<sha> # vX.Y.Z") so Dependabot can still update them.
func TestThirdPartyActionsArePinned(t *testing.T) {
	files, err := filepath.Glob(".github/workflows/*.yml")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	uses := regexp.MustCompile(`uses:\s*([\w.-]+/[\w./-]+)@(\S+)`)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // f comes from a glob over this repository's workflow files
		require.NoError(t, err)
		for _, m := range uses.FindAllStringSubmatch(string(raw), -1) {
			action, ref := m[1], m[2]
			if strings.HasPrefix(action, "actions/") {
				continue // GitHub's own actions
			}
			assert.Regexp(t, sha, ref, "%s: %s must be pinned to a commit SHA", f, action)
		}
	}
}

func validateShipped(t *testing.T, name, yaml string) {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(yaml)), name)
	cfg, err := atc.LoadConfigFromViper(v)
	require.NoError(t, err, name)
	if len(cfg.Target) == 0 {
		cfg.Target = []string{"all"} // the CLI default
	}
	assert.Empty(t, atc.ValidateConfig(cfg), "%s must pass validation", name)
}

// TestShippedConfigsAreValid: every configuration the project ships must start.
func TestShippedConfigsAreValid(t *testing.T) {
	raw, err := os.ReadFile("deploy/strategies.yaml")
	require.NoError(t, err)
	validateShipped(t, "deploy/strategies.yaml", string(raw))

	values := viper.New()
	values.SetConfigFile("deploy/helm/atc/values.yaml")
	require.NoError(t, values.ReadInConfig())
	validateShipped(t, "Helm config.strategiesYaml", values.GetString("config.strategiesYaml"))

	nomad, err := os.ReadFile("deploy/nomad/atc.nomad.hcl")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?s)data = <<EOF\n(.*?)\nEOF`).FindStringSubmatch(string(nomad))
	require.NotNil(t, m, "Nomad job must embed the config in a template block")
	tpl := regexp.MustCompile(`\$\{var\.\w+\}`).ReplaceAllStringFunc(m[1], func(v string) string {
		switch v {
		case "${var.dry_run}":
			return "false"
		case "${var.write_rate_limit}":
			return "1s"
		default:
			return ""
		}
	})
	validateShipped(t, "Nomad job template", tpl)
}
