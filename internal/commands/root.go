// Package commands defines every axon verb.
package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/layout"
	"axon/internal/ui"
)

// Build metadata (set with -ldflags "-X axon/internal/commands.Version=…").
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
	Date    = ""
)

// Registry builds the full verb registry.
func Registry() *cli.Registry {
	r := cli.New("axon")
	r.Version = Version
	r.Tagline = "build, deploy and operate app clusters (Docker Compose or Kubernetes) from one descriptor"
	r.Globals = []cli.Flag{
		{Name: "help", Short: "h", Kind: cli.Bool, Usage: "show help"},
		{Name: "version", Kind: cli.Bool, Usage: "print the version"},
		{Name: "yes", Short: "y", Kind: cli.Bool, Usage: "assume yes for confirmations (required without a TTY)"},
		{Name: "quiet", Short: "q", Kind: cli.Bool, Usage: "only print results and errors"},
		{Name: "verbose", Short: "v", Kind: cli.Bool, Usage: "print every external command"},
		{Name: "no-color", Kind: cli.Bool, Usage: "disable color (also NO_COLOR)"},
		{Name: "compat", Kind: cli.String, Value: "<mode>", Usage: "auto (default): axon files, else mycel files · mycel: prefer and scaffold mycel names · off: ignore mycel files (also AXON_COMPAT)"},
	}
	r.Common = []string{"up", "down", "cluster new", "cluster add app", "cluster check", "app new", "env status", "env logs", "env test", "release create"}
	r.Add(topVerbs()...)
	r.Add(clusterVerbs()...)
	r.Add(appVerbs()...)
	r.Add(envVerbs()...)
	r.Add(releaseVerbs()...)
	// mycel's moved names keep working
	r.AddMoved(&cli.Moved{Old: "platform apply", New: "env deploy"})
	r.AddMoved(&cli.Moved{Old: "platform destroy", New: "env undeploy"})
	r.AddMoved(&cli.Moved{Old: "platform init", New: "env new"})
	r.AddMoved(&cli.Moved{Old: "platform outputs", New: "env show"})
	r.AddMoved(&cli.Moved{Old: "platform plan", New: "env plan"})
	r.AddMoved(&cli.Moved{Old: "platform status", New: "env status"})
	r.AddMoved(&cli.Moved{Old: "platform upgrade", New: "env set"})
	r.AddMoved(&cli.Moved{Old: "add app", New: "cluster add app"})
	r.AddMoved(&cli.Moved{Old: "add ext", New: "cluster add ext"})
	r.AddMoved(&cli.Moved{Old: "add-fabric", New: "cluster add fabric"})
	return r
}

// ── shared flags ────────────────────────────────────────────────────────────

var (
	flagDir    = cli.Flag{Name: "dir", Kind: cli.String, Value: "<path>", Usage: "cluster or app directory", Default: ""}
	flagJSON   = cli.Flag{Name: "json", Kind: cli.Bool, Usage: "print one JSON document on stdout"}
	flagDryRun = cli.Flag{Name: "dry-run", Kind: cli.Bool, Usage: "print the plan; change nothing"}
	flagForce  = cli.Flag{Name: "force", Kind: cli.Bool, Usage: "take over the cluster lock even if another run holds it"}
	flagCheck  = cli.Flag{Name: "check", Kind: cli.Bool, Usage: "validate only; write nothing (exit 1 if something would change)"}
)

func compat(c *cli.Ctx) string {
	m, err := layout.NormalizeMode(c.G.Compat)
	if err != nil {
		ui.Warn("%v; using auto", err)
		return layout.ModeAuto
	}
	return m
}

// dirArg returns the positional dir at index i, else --dir, else ".".
func dirArg(c *cli.Ctx, i int) string {
	if d := c.Arg(i); d != "" {
		return d
	}
	if d := c.String("dir"); d != "" {
		return d
	}
	return "."
}

// loadCluster finds and resolves the cluster for dir (searching upward).
func loadCluster(c *cli.Ctx, dir string) (*descriptor.Model, error) {
	f, err := layout.FindClusterUpward(dir, compat(c))
	if err != nil {
		abs, _ := filepath.Abs(dir)
		return nil, cli.NoContext("no cluster axonfile (or mycelfile) in %s — `%s` runs in a cluster controller directory; pass --dir <path>", abs, c.Verb.Name)
	}
	m, err := descriptor.Load(f)
	if err != nil {
		return nil, err
	}
	if f.Layout.Compat() {
		ui.Debug("compat: using mycel layout (%s)", f.Path)
	}
	return m, nil
}

func versionString() string {
	date := Date
	if date == "" {
		if exe, err := os.Executable(); err == nil {
			if st, err := os.Stat(exe); err == nil {
				date = st.ModTime().UTC().Format(time.RFC3339)
			}
		}
	}
	commit := Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf("axon %s — built from %s at %s", Version, commit, date)
}

func topVerbs() []*cli.Verb {
	return []*cli.Verb{
		{Name: "version", Summary: "print the version", Group: "general", Run: func(c *cli.Ctx) error {
			ui.Out("%s", versionString())
			return nil
		}},
		{Name: "completion", Args: []cli.Arg{{Name: "shell"}}, Group: "general",
			Summary: "print a shell completion script (bash, zsh or fish), generated from the verb registry",
			Help:    "Print a completion script generated from the live verb registry.\n  bash: source <(axon completion bash)\n  zsh:  axon completion zsh > \"${fpath[1]}/_axon\"\n  fish: axon completion fish > ~/.config/fish/completions/axon.fish",
			Run: func(c *cli.Ctx) error {
				s, err := c.R.Completion(c.Arg(0))
				if err != nil {
					return err
				}
				ui.Raw(s)
				return nil
			}},
		upVerb(), downVerb(), migrateVerb(),
	}
}
