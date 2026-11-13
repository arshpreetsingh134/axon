package commands

import (
	"fmt"
	"strings"

	"axon/internal/ci"
	"axon/internal/cigen"
	"axon/internal/cli"
	"axon/internal/ui"
)

func ciEnvironmentsVerb() *cli.Verb {
	return &cli.Verb{Name: "ci environments", Group: "ci",
		Summary: "show the GitHub Environments the deploy pipeline uses and (--apply) create them with their approvals",
		Help: "Every environment file with [ci] deploy = true becomes a job in .github/workflows/axon-deploy.yml bound to a " +
			"GitHub Environment of the same name. This lists them with their protection — required reviewers, wait timer, " +
			"allowed branches (from [ci] reviewers / wait_minutes / branches in environments/<env>.toml) — and whether GitHub " +
			"has them. --apply creates or updates them through the GitHub API. Reviewers are user logins or org/team slugs.",
		Flags: []cli.Flag{flagDir, flagJSON,
			{Name: "apply", Kind: cli.Bool, Usage: "create or update the GitHub Environments"},
			{Name: "remote", Kind: cli.String, Default: "origin", Usage: "git remote that points at GitHub"}},
		Run: runCIEnvironments}
}

func runCIEnvironments(c *cli.Ctx) error {
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
	if len(res.Envs) == 0 {
		if c.Bool("json") {
			return ui.JSON(map[string]any{"environments": []any{}})
		}
		ui.Out("no environment deploys from CI — set [ci] deploy = true (and [cloud]) in environments/<env>.toml, then `axon ci init`")
		return nil
	}
	repo, err := ci.OpenRepo(m.Dir, c.String("remote"))
	if err != nil {
		return err
	}
	gh := ci.NewGitHub(repo.Owner, repo.Name)
	have, listErr := gh.EnvironmentNames()
	type row struct {
		Name         string   `json:"name"`
		Reviewers    []string `json:"reviewers"`
		WaitMinutes  int      `json:"waitMinutes"`
		Branches     []string `json:"branches"`
		PromotesFrom string   `json:"promotesFrom,omitempty"`
		Auto         bool     `json:"auto"`
		OnGitHub     *bool    `json:"onGitHub"`
	}
	var rows []row
	for _, e := range res.Envs {
		r := row{Name: e.Name, Reviewers: e.Reviewers, WaitMinutes: e.WaitMinutes, Branches: e.Branches, PromotesFrom: e.PromotesFrom, Auto: e.Auto}
		if listErr == nil {
			on := have[e.Name]
			r.OnGitHub = &on
		}
		rows = append(rows, r)
	}
	if c.Bool("json") && !c.Bool("apply") {
		return ui.JSON(map[string]any{"repository": repo.Owner + "/" + repo.Name, "environments": rows})
	}
	ui.Out("%s/%s — GitHub Environments of the deploy pipeline", repo.Owner, repo.Name)
	ui.Out("")
	table := [][]string{{"ENVIRONMENT", "TRIGGER", "REVIEWERS", "WAIT", "BRANCHES", "ON GITHUB"}}
	for _, r := range rows {
		trigger := "manual (workflow_dispatch)"
		switch {
		case r.Auto:
			trigger = "every release"
		case r.PromotesFrom != "":
			trigger = "after " + r.PromotesFrom
		}
		wait := ui.Dim("—")
		if r.WaitMinutes > 0 {
			wait = fmt.Sprintf("%dm", r.WaitMinutes)
		}
		on := ui.Dim("?")
		if r.OnGitHub != nil {
			on = ui.Green("✓")
			if !*r.OnGitHub {
				on = ui.Red("✗ missing")
			}
		}
		table = append(table, []string{r.Name, trigger, or(strings.Join(r.Reviewers, ", "), ui.Dim("none")), wait,
			or(strings.Join(r.Branches, ", "), ui.Dim("any")), on})
	}
	ui.Table(ui.Stdout, table, true)
	if !c.Bool("apply") {
		if listErr != nil {
			ui.Warn("could not list the repository's environments: %v", listErr)
		}
		ui.Out("\n`axon ci environments --apply` creates or updates them (existing protection is replaced by the configured one)")
		return nil
	}
	if !ui.Confirm(fmt.Sprintf("Create or update %d GitHub Environment(s) on %s/%s?", len(rows), repo.Owner, repo.Name), c.G.Yes) {
		return fmt.Errorf("not changing GitHub without confirmation (pass --yes)")
	}
	for _, e := range res.Envs {
		if err := gh.PutEnvironment(ci.Environment{Name: e.Name, WaitMinutes: e.WaitMinutes, Reviewers: e.Reviewers, Branches: e.Branches}); err != nil {
			return fmt.Errorf("environment %s: %w", e.Name, err)
		}
		ui.OK("environment %s", e.Name)
	}
	if len(res.EnvSecrets) > 0 {
		ui.Out("\nnext: `axon ci secrets --kubeconfig <file> --set` uploads the environments' KUBECONFIG")
	}
	return nil
}
