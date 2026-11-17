package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

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

// releaseContext holds the GoReleaser template fields the image tag templates use.
type releaseContext struct {
	Version    string
	Major      uint64
	Minor      uint64
	Prerelease string
	IsNightly  bool
}

func renderTags(t *testing.T, templates []string, ctx releaseContext) []string {
	t.Helper()
	var tags []string
	for _, src := range templates {
		tpl, err := template.New("tag").Parse(src)
		require.NoError(t, err, src)
		var b strings.Builder
		require.NoError(t, tpl.Execute(&b, ctx), src)
		if tag := strings.TrimSpace(b.String()); tag != "" { // GoReleaser skips empty tags
			tags = append(tags, tag)
		}
	}
	return tags
}

// TestImageTagsFollowVersioningPolicy renders the real image tag templates from
// .goreleaser.yaml: artifacts use X.Y.Z without a "v", and the floating tags (X.Y, X,
// latest) only move for full releases.
func TestImageTagsFollowVersioningPolicy(t *testing.T) {
	v := viper.New()
	v.SetConfigFile(".goreleaser.yaml")
	require.NoError(t, v.ReadInConfig())
	dockers, ok := v.Get("dockers_v2").([]any)
	require.True(t, ok && len(dockers) == 1, "expected one dockers_v2 entry")
	var templates []string
	for _, tag := range dockers[0].(map[string]any)["tags"].([]any) {
		templates = append(templates, tag.(string))
	}

	assert.Equal(t, []string{"0.5.0", "0.5", "0", "latest"},
		renderTags(t, templates, releaseContext{Version: "0.5.0", Major: 0, Minor: 5}), "full release")
	assert.Equal(t, []string{"0.5.0-rc.1"},
		renderTags(t, templates, releaseContext{Version: "0.5.0-rc.1", Major: 0, Minor: 5, Prerelease: "rc.1"}),
		"pre-releases must not move latest or the floating tags")
	assert.Equal(t, []string{"0.5.1-nightly"},
		renderTags(t, templates, releaseContext{Version: "0.5.1-nightly", Major: 0, Minor: 5, IsNightly: true}))
	for _, tpl := range templates {
		assert.NotContains(t, tpl, "v{{", "image tags must not carry a v prefix")
	}
}

// TestReleaseConfigFollowsVersioningPolicy checks the other release settings of the policy.
func TestReleaseConfigFollowsVersioningPolicy(t *testing.T) {
	v := viper.New()
	v.SetConfigFile(".goreleaser.yaml")
	require.NoError(t, v.ReadInConfig())

	assert.NotContains(t, v.GetString("snapshot.version_template"), ".Tag", "snapshot versions must be plain X.Y.Z")
	casks, ok := v.Get("homebrew_casks").([]any)
	require.True(t, ok && len(casks) > 0)
	for _, c := range casks {
		assert.Equal(t, "auto", c.(map[string]any)["skip_upload"], "pre-releases must not be published to Homebrew")
	}

	makefile, err := os.ReadFile("Makefile")
	require.NoError(t, err)
	assert.Contains(t, string(makefile), "svu next --v0", "make tag must not jump to 1.0.0 on a breaking change")
}

// TestChangelogGroupsBreakingChanges: PR titles marked with "!" land under Breaking changes.
func TestChangelogGroupsBreakingChanges(t *testing.T) {
	v := viper.New()
	v.SetConfigFile(".goreleaser.yaml")
	require.NoError(t, v.ReadInConfig())
	groups, ok := v.Get("changelog.groups").([]any)
	require.True(t, ok && len(groups) > 0)

	// GoReleaser puts a commit in the first group (by order) whose regexp matches.
	groupOf := func(subject string) string {
		for _, g := range groups {
			m := g.(map[string]any)
			re, _ := m["regexp"].(string)
			if re == "" || regexp.MustCompile(re).MatchString(subject) {
				return m["title"].(string)
			}
		}
		return ""
	}
	assert.Equal(t, "Breaking changes", groupOf("feat(auth)!: require auth on network addresses (#181)"))
	assert.Equal(t, "Breaking changes", groupOf("fix!: drop the v0 image tag"))
	assert.Equal(t, "Features", groupOf("feat: readiness reflects Consul reachability"))
	assert.Equal(t, "Fixes", groupOf("fix(deploy): published chart image tag"))
	assert.Equal(t, "Other changes", groupOf("chore(deps): Bump grpc"))
}
