package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"axon/internal/ci"
	"axon/internal/cigen"
	"axon/internal/cli"
	"axon/internal/dotenv"
	"axon/internal/secrets"
	"axon/internal/ui"
)

func ciSecretsVerb() *cli.Verb {
	return &cli.Verb{Name: "ci secrets", Group: "ci",
		Summary: "list the secrets the generated pipelines need, whether GitHub has them, and (--set) upload missing ones",
		Help: "Show every secret the generated workflows read, whether the repository already has it on GitHub, and where its value " +
			"would come from locally (axon secrets, then .env, then the environment). Deploy environments whose [cloud] provider is " +
			"\"kubeconfig\" also need a KUBECONFIG secret in their GitHub Environment; its value comes from --kubeconfig <file>. With --set, upload the ones GitHub lacks " +
			"(--update also overwrites existing ones), encrypted for the repository with GitHub's public key. Values are never " +
			"printed. Without a terminal, --set needs --yes.",
		Flags: []cli.Flag{flagDir, flagJSON,
			{Name: "env", Kind: cli.String, Value: "<env>", Usage: "which environment's axon secrets supply values (default [deploy].env, else dev)"},
			{Name: "context", Kind: cli.String, Value: "<kube-context>", Usage: "kube context holding the axon secret store"},
			{Name: "set", Kind: cli.Bool, Usage: "upload missing secrets that have a local value"},
			{Name: "update", Kind: cli.Bool, Usage: "with --set: also overwrite secrets GitHub already has"},
			{Name: "only", Kind: cli.String, Value: "<A,B>", Usage: "limit to these names"},
			{Name: "kubeconfig", Kind: cli.String, Value: "<file>", Usage: "kubeconfig to upload as the KUBECONFIG secret of environments with [cloud] provider = \"kubeconfig\""},
			{Name: "remote", Kind: cli.String, Default: "origin", Usage: "git remote that points at GitHub"}},
		Run: runCISecrets}
}

type secretRow struct {
	Name     string `json:"name"`
	Env      string `json:"environment,omitempty"` // GitHub Environment ("" = repository)
	Required bool   `json:"required"`
	OnGitHub bool   `json:"onGitHub"`
	Source   string `json:"localSource"` // axon-secrets | .env | environment | ""
	value    string
}

func runCISecrets(c *cli.Ctx) error {
	m, err := loadCluster(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	_, lay, err := ciLayout(m)
	if err != nil {
		return err
	}
	res, err := cigen.Generate(m, lay)
	if err != nil {
		return err
	}
	repo, err := ci.OpenRepo(m.Dir, c.String("remote"))
	if err != nil {
		return err
	}
	gh := ci.NewGitHub(repo.Owner, repo.Name)
	have, err := gh.SecretNames()
	if err != nil {
		return fmt.Errorf("list the repository's secrets: %w", err)
	}
	// local value sources, in order
	sources := []struct {
		name string
		vals map[string]string
	}{}
	if s, err := openSession(m, deployOpts{Verb: "ci secrets", Env: c.String("env"), Context: c.String("context"), Target: "docker"}); err == nil {
		if st := s.SecretStore(); st.Available() {
			if v, err := st.Values(); err == nil {
				sources = append(sources, struct {
					name string
					vals map[string]string
				}{"axon-secrets (" + secrets.Namespace + "/" + st.Name + ")", v})
			}
		}
	}
	if v, err := dotenv.Read(filepath.Join(m.Infra, ".env")); err == nil {
		sources = append(sources, struct {
			name string
			vals map[string]string
		}{".env", v})
	}
	only := map[string]bool{}
	for _, n := range strings.Split(c.String("only"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			only[n] = true
		}
	}
	var rows []secretRow
	add := func(name string, required bool) {
		if len(only) > 0 && !only[name] {
			return
		}
		r := secretRow{Name: name, Required: required, OnGitHub: have[name]}
		for _, s := range sources {
			if v := s.vals[name]; v != "" {
				r.Source, r.value = s.name, v
				break
			}
		}
		if r.Source == "" {
			if v := os.Getenv(name); v != "" {
				r.Source, r.value = "environment", v
			}
		}
		rows = append(rows, r)
	}
	for _, s := range res.Secrets {
		add(s, true)
	}
	for _, s := range res.Optional {
		add(s, false)
	}
	// secrets scoped to a GitHub Environment (the deploy jobs' cluster credentials)
	var kubeconfig string
	if f := c.String("kubeconfig"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		kubeconfig = string(b)
	}
	for _, env := range sortedKeysOfSlice(res.EnvSecrets) {
		envHave, err := gh.EnvSecretNames(env)
		var apiErr *ci.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			envHave = map[string]bool{} // the Environment does not exist yet
		} else if err != nil {
			return fmt.Errorf("list environment %s's secrets: %w", env, err)
		}
		for _, name := range res.EnvSecrets[env] {
			if len(only) > 0 && !only[name] {
				continue
			}
			r := secretRow{Name: name, Env: env, Required: true, OnGitHub: envHave[name]}
			if name == "KUBECONFIG" && kubeconfig != "" {
				r.Source, r.value = "--kubeconfig "+c.String("kubeconfig"), kubeconfig
			}
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Required && !rows[j].Required })
	if c.Bool("json") && !c.Bool("set") {
		if rows == nil {
			rows = []secretRow{}
		}
		return ui.JSON(map[string]any{"repository": repo.Owner + "/" + repo.Name, "secrets": rows})
	}
	ui.Out("%s/%s — secrets read by the generated pipelines", repo.Owner, repo.Name)
	if len(rows) == 0 {
		ui.Out("none needed")
		return nil
	}
	table := [][]string{{"SECRET", "SCOPE", "NEEDED", "ON GITHUB", "LOCAL VALUE FROM"}}
	missing := 0
	for _, r := range rows {
		need := "required"
		if !r.Required {
			need = "optional"
		}
		on := ui.Green("✓")
		if !r.OnGitHub {
			on = ui.Red("✗ missing")
			if r.Required {
				missing++
			} else {
				on = ui.Dim("·")
			}
		}
		scope := "repository"
		if r.Env != "" {
			scope = "env " + r.Env
		}
		table = append(table, []string{r.Name, scope, need, on, or(r.Source, ui.Dim("(none)"))})
	}
	ui.Out("")
	ui.Table(ui.Stdout, table, true)
	if !c.Bool("set") {
		if missing > 0 {
			ui.Out("\n%d required secret(s) missing on GitHub — `axon ci secrets --set` uploads those with a local value", missing)
			return cli.Failed()
		}
		return nil
	}
	// upload
	var todo []secretRow
	var noValue []string
	for _, r := range rows {
		if r.OnGitHub && !c.Bool("update") {
			continue
		}
		if r.value == "" {
			if r.Required && !r.OnGitHub {
				noValue = append(noValue, r.Name)
			}
			continue
		}
		todo = append(todo, r)
	}
	if len(todo) == 0 {
		ui.Out("\nnothing to upload")
	} else {
		var names []string
		for _, r := range todo {
			where := ""
			if r.Env != "" {
				where = " → environment " + r.Env
			}
			names = append(names, fmt.Sprintf("%s%s (from %s)", r.Name, where, r.Source))
		}
		ui.Out("\nwill upload to %s/%s:\n  %s", repo.Owner, repo.Name, strings.Join(names, "\n  "))
		if !ui.Confirm("Upload these secrets to GitHub?", c.G.Yes) {
			return fmt.Errorf("not uploading without confirmation (pass --yes)")
		}
		for _, r := range todo {
			if err := gh.SetEnvSecret(r.Env, r.Name, r.value); err != nil {
				var apiErr *ci.APIError
				if r.Env != "" && errors.As(err, &apiErr) && apiErr.Status == 404 {
					return fmt.Errorf("%s: GitHub Environment %s does not exist yet — run `axon ci environments --apply` first", r.Name, r.Env)
				}
				return fmt.Errorf("%s: %w", r.Name, err)
			}
			if r.Env != "" {
				ui.OK("set %s in environment %s", r.Name, r.Env)
			} else {
				ui.OK("set %s", r.Name)
			}
		}
	}
	if len(noValue) > 0 {
		ui.Warn("no local value for %s — `axon secrets set <NAME>` (KUBECONFIG: --kubeconfig <file>), then rerun", strings.Join(noValue, ", "))
		return cli.Failed()
	}
	return nil
}

func sortedKeysOfSlice(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
