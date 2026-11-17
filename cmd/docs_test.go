package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryFlagIsDocumented keeps the reference site's flag table in step with the CLI: each
// flag must be listed together with the ATC_* environment variable viper derives from it.
func TestEveryFlagIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../docs/index.html")
	require.NoError(t, err)
	docs := string(raw)
	ws := regexp.MustCompile(`\s+`)

	check := func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		env := "ATC_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		row := ws.ReplaceAllString(`<td><code>--`+f.Name+`</code></td> <td><code>`+env+`</code></td>`, " ")
		assert.True(t, strings.Contains(ws.ReplaceAllString(docs, " "), row), "flag --%s must be documented with %s", f.Name, env)
	}
	rootCmd.PersistentFlags().VisitAll(check)
	serverCmd.PersistentFlags().VisitAll(check)

	assert.False(t, strings.Contains(docs, `write_rate_limit: "10/s"`), "write_rate_limit is a duration, not a rate")
}
