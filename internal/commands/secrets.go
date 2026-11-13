package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"

	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/dotenv"
	"axon/internal/secrets"
	"axon/internal/sh"
	"axon/internal/ui"
)

func secretsVerbs() []*cli.Verb {
	envFlag := cli.Flag{Name: "env", Kind: cli.String, Value: "<env>", Usage: "environment (default [deploy].env, else dev)"}
	ctxFlag := cli.Flag{Name: "context", Kind: cli.String, Value: "<kube-context>", Usage: "kube context that holds the store (default: the environment's, else current)"}
	return []*cli.Verb{
		{Name: "secrets set", Args: []cli.Arg{{Name: "name"}}, Group: "secrets",
			Summary: "store a secret (prompted with hidden input, or piped on stdin); apps read it as ${NAME}",
			Help: "Store a secret value in axon's secret store: a Kubernetes Secret in the axon-secrets namespace, one per " +
				"project and environment. The value is read from a hidden prompt, or from stdin when piped " +
				"(e.g. `pbpaste | axon secrets set OPENROUTER_API_KEY`). It is never accepted on the command line, " +
				"never written to a file and never printed. Reference it from an app as KEY = \"${NAME}\" under [app.env]; " +
				"deploys inject it as an environment variable (os.Getenv(\"NAME\") in Go).",
			Flags: []cli.Flag{flagDir, envFlag, ctxFlag}, Run: runSecretsSet},
		{Name: "secrets list", Group: "secrets", Summary: "list stored secret names, when they changed and which apps use them (never values)",
			Flags: []cli.Flag{flagDir, envFlag, ctxFlag, flagJSON}, Run: runSecretsList},
		{Name: "secrets rm", Args: []cli.Arg{{Name: "name", Variadic: true}}, Group: "secrets", Summary: "delete stored secrets",
			Flags: []cli.Flag{flagDir, envFlag, ctxFlag}, Run: runSecretsRm},
		{Name: "secrets import", Group: "secrets", Summary: "move values from .env into the secret store (--remove deletes them from .env)",
			Flags: []cli.Flag{flagDir, envFlag, ctxFlag,
				{Name: "keys", Kind: cli.String, Value: "<A,B>", Usage: "only these keys (default: every key in .env)"},
				{Name: "remove", Kind: cli.Bool, Usage: "delete the imported keys from .env afterwards"}},
			Run: runSecretsImport},
		{Name: "secrets exec", Args: []cli.Arg{{Name: "command", Variadic: true}}, Group: "secrets",
			Summary: "run a local command with the project's secrets as environment variables (axon secrets exec -- go run .)",
			Flags:   []cli.Flag{flagDir, envFlag, ctxFlag}, Run: runSecretsExec},
	}
}

// openStore resolves the project, environment and kube context of the store.
func openStore(c *cli.Ctx) (*descriptor.Model, *deploy.Session, *secrets.Store, error) {
	m, err := controller(c)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := openSession(m, deployOpts{Verb: c.Verb.Name, Env: c.String("env"), Context: c.String("context"), Target: "docker"})
	if err != nil {
		return nil, nil, nil, err
	}
	st := s.SecretStore()
	if !st.Available() {
		where := "the current kube context"
		if st.Context != "" {
			where = "kube context " + st.Context
		}
		return nil, nil, nil, fmt.Errorf("the secret store lives in Kubernetes, but %s is unreachable: turn on Kubernetes in Docker Desktop/OrbStack, or pass --context", where)
	}
	return m, s, st, nil
}

func readSecretValue(name string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(ui.Stderr, "Value for %s (input hidden): ", name)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(ui.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
	if err != nil {
		return "", err
	}
	if len(b) > 1<<20 {
		return "", errors.New("value is larger than 1 MiB")
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

func runSecretsSet(c *cli.Ctx) error {
	name := c.Arg(0)
	if strings.Contains(name, "=") {
		return cli.Usage("secrets set: pass only the name; the value is prompted (hidden) or read from stdin, never from the command line, where it would land in shell history")
	}
	if !secrets.ValidName(name) {
		return cli.Usage("secrets set: %q is not a valid name (use UPPER_SNAKE_CASE, like an environment variable)", name)
	}
	_, s, st, err := openStore(c)
	if err != nil {
		return err
	}
	v, err := readSecretValue(name)
	if err != nil {
		return err
	}
	if v == "" {
		return fmt.Errorf("empty value; nothing stored")
	}
	if err := st.Set(map[string]string{name: v}); err != nil {
		return err
	}
	ui.OK("stored %s for %s/%s (%s/%s)", name, s.M.C.Name, s.Env.Name, secrets.Namespace, st.Name)
	if users := secretUsers(s.M)[name]; len(users) == 0 {
		ui.Note("no app references it yet: add %s = \"${%s}\" under [app.env] of the app that needs it", name, name)
	} else {
		ui.Note("used by %s; redeploy to roll it out (`axon up`)", strings.Join(users, ", "))
	}
	return nil
}

// secretUsers maps each ${NAME} reference to the apps whose env uses it.
func secretUsers(m *descriptor.Model) map[string][]string {
	out := map[string][]string{}
	for _, a := range m.C.Apps {
		seen := map[string]bool{}
		for _, v := range a.Env {
			for _, r := range dotenv.Refs(v) {
				if !seen[r] {
					seen[r] = true
					out[r] = append(out[r], a.Name)
				}
			}
		}
	}
	return out
}

func runSecretsList(c *cli.Ctx) error {
	m, s, st, err := openStore(c)
	if err != nil {
		return err
	}
	list, err := st.List()
	if err != nil {
		return err
	}
	users := secretUsers(m)
	stored := map[string]bool{}
	for _, x := range list {
		stored[x.Name] = true
	}
	envVals, _ := dotenv.Read(filepath.Join(m.Infra, ".env"))
	var missing []string
	for n := range users {
		if !stored[n] {
			if _, inEnv := envVals[n]; !inEnv {
				missing = append(missing, n)
			}
		}
	}
	sort.Strings(missing)
	if c.Bool("json") {
		type row struct {
			secrets.Meta
			UsedBy []string `json:"usedBy"`
		}
		rows := []row{}
		for _, x := range list {
			rows = append(rows, row{x, append([]string{}, users[x.Name]...)})
		}
		if missing == nil {
			missing = []string{}
		}
		return ui.JSON(map[string]any{"project": m.C.Name, "env": s.Env.Name, "store": secrets.Namespace + "/" + st.Name, "secrets": rows, "referencedButMissing": missing})
	}
	ui.Out("%s/%s — store %s/%s", m.C.Name, s.Env.Name, secrets.Namespace, st.Name)
	if len(list) == 0 {
		ui.Out("no secrets stored yet — `axon secrets set <NAME>` or `axon secrets import`")
	} else {
		rows := [][]string{{"NAME", "UPDATED", "USED BY"}}
		for _, x := range list {
			u := strings.Join(users[x.Name], ", ")
			if u == "" {
				u = ui.Dim("(no app references it)")
			}
			rows = append(rows, []string{x.Name, or(x.Updated, "-"), u})
		}
		ui.Out("")
		ui.Table(ui.Stdout, rows, true)
	}
	if len(missing) > 0 {
		ui.Out("")
		for _, n := range missing {
			ui.Out("%s %s is referenced by %s but not set — axon secrets set %s", ui.Yellow("!"), n, strings.Join(users[n], ", "), n)
		}
	}
	return nil
}

func runSecretsRm(c *cli.Ctx) error {
	_, _, st, err := openStore(c)
	if err != nil {
		return err
	}
	missing, err := st.Delete(c.Args...)
	if err != nil {
		return err
	}
	gone := map[string]bool{}
	for _, n := range missing {
		gone[n] = true
		ui.Warn("%s was not stored", n)
	}
	for _, n := range c.Args {
		if !gone[n] {
			ui.OK("deleted %s", n)
		}
	}
	if len(missing) == len(c.Args) {
		return cli.Failed()
	}
	return nil
}

func runSecretsImport(c *cli.Ctx) error {
	m, s, st, err := openStore(c)
	if err != nil {
		return err
	}
	envPath := filepath.Join(m.Infra, ".env")
	vals, err := dotenv.Read(envPath)
	if err != nil {
		return err
	}
	want := map[string]string{}
	if k := c.String("keys"); k != "" {
		for _, n := range strings.Split(k, ",") {
			n = strings.TrimSpace(n)
			v, ok := vals[n]
			if !ok {
				return fmt.Errorf("%s is not in %s", n, rel2(m.Dir, envPath))
			}
			want[n] = v
		}
	} else {
		for k, v := range vals {
			if v != "" {
				want[k] = v
			}
		}
	}
	var skipped []string
	for k := range want {
		if !secrets.ValidName(k) {
			skipped = append(skipped, k)
			delete(want, k)
		}
	}
	if len(want) == 0 {
		return fmt.Errorf("nothing to import from %s", rel2(m.Dir, envPath))
	}
	if err := st.Set(want); err != nil {
		return err
	}
	names := dotenv.Keys(want)
	ui.OK("imported %d secret(s) into %s/%s for %s/%s: %s", len(names), secrets.Namespace, st.Name, m.C.Name, s.Env.Name, strings.Join(names, ", "))
	for _, k := range skipped {
		ui.Warn("skipped %s (not a valid environment variable name)", k)
	}
	if c.Bool("remove") {
		if err := dotenv.Remove(envPath, names...); err != nil {
			return err
		}
		ui.OK("removed them from %s", rel2(m.Dir, envPath))
	} else {
		ui.Note("they are still in %s too — rerun with --remove to delete them there", rel2(m.Dir, envPath))
	}
	return nil
}

func runSecretsExec(c *cli.Ctx) error {
	if len(c.Args) == 0 {
		return cli.Usage("secrets exec: missing required argument(s): command (e.g. axon secrets exec -- go run .)")
	}
	m, _, st, err := openStore(c)
	if err != nil {
		return err
	}
	env, _ := dotenv.Read(filepath.Join(m.Infra, ".env"))
	stored, err := st.Values()
	if err != nil {
		return err
	}
	for k, v := range stored {
		env[k] = v
	}
	var kv []string
	for k, v := range env {
		kv = append(kv, k+"="+v)
	}
	wd, _ := os.Getwd()
	if err := sh.Interactive(wd, kv, c.Args[0], c.Args[1:]...); err != nil {
		var se *sh.Error
		if errors.As(err, &se) && se.Code > 0 {
			return &cli.ExitError{Code: se.Code, Silent: true}
		}
		return err
	}
	return nil
}
