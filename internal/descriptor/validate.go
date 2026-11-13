package descriptor

import (
	"fmt"
	"strings"
)

// Problem is a validation finding.
type Problem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // blocking | risk | note
	Subject  string `json:"subject"`
	Cause    string `json:"cause"`
	Fix      string `json:"fix,omitempty"`
}

// ValidateCluster checks a cluster descriptor and its resolved apps.
func ValidateCluster(m *Model) []Problem {
	var ps []Problem
	add := func(id, sev, subj, cause, fix string) {
		ps = append(ps, Problem{ID: id, Severity: sev, Subject: subj, Cause: cause, Fix: fix})
	}
	c := m.C
	if c.Name == "" {
		add("CONFORMANCE_NAME_MISSING", "blocking", "cluster", "name is empty", "set name = \"<cluster>\"")
	} else if !ValidLabel(strings.ToLower(c.Name)) {
		add("CONFORMANCE_NAME_NOT_DNS_SAFE", "blocking", "cluster", fmt.Sprintf("name %q is not a DNS label", c.Name), "use lowercase letters, digits and '-'")
	}
	if c.Fabric.Postgres == nil {
		add("CONFORMANCE_POSTGRES_MISSING", "blocking", "fabric", "[fabric.postgres] is required", "axon cluster add fabric postgres")
	}
	if c.Fabric.Traefik == nil {
		add("CONFORMANCE_TRAEFIK_MISSING", "risk", "fabric", "[fabric.traefik] is missing; apps have no front door", "add [fabric.traefik] http = <port>")
	}
	seenName, seenHost, seenTok := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, a := range c.Apps {
		subj := "app " + a.Name
		if seenName[a.Name] {
			add("CONFORMANCE_DUPLICATE_APP", "blocking", subj, "declared twice", "remove one [[app]] block")
		}
		seenName[a.Name] = true
		switch a.KindOrDefault() {
		case "module", "iam", "project":
		default:
			add("CONFORMANCE_APP_KIND", "blocking", subj, fmt.Sprintf("unknown kind %q", a.Kind), "kind = module | iam | project")
		}
		if a.Cored() {
			if a.Host == "" {
				add("CONFORMANCE_APP_HOST_MISSING", "blocking", subj, "cored app has no host", "set host = \"<label>\"")
			} else if !ValidLabel(a.Host) {
				add("CONFORMANCE_APP_HOST_NOT_DNS_SAFE", "blocking", subj, fmt.Sprintf("host %q is not a DNS label", a.Host), "use lowercase letters, digits and '-'")
			} else if other, dup := seenHost[a.Host]; dup {
				add("CONFORMANCE_DUPLICATE_HOST", "blocking", subj, fmt.Sprintf("host %q is also used by %s", a.Host, other), "pick a unique host")
			} else {
				seenHost[a.Host] = a.Name
			}
		}
		if a.KindOrDefault() == "project" && a.Temporal != "" && c.Fabric.Temporal == nil {
			add("CONFORMANCE_TEMPORAL_MISSING", "blocking", subj, "declares temporal but [fabric.temporal] is absent", "axon cluster add fabric temporal")
		}
		if a.Kind == "iam" && c.Fabric.IAM == nil {
			add("CONFORMANCE_IAM_CONTRACT_MISSING", "risk", subj, "an iam app without [fabric.iam]", "add [fabric.iam] issuer/audience/backend")
		}
	}
	for _, am := range m.Apps {
		if am.Problem != "" {
			add("CONFORMANCE_APP_NOT_PRESENT", "blocking", "app "+am.Decl.Name, am.Problem, "clone or link the app next to the cluster dir")
			continue
		}
		for _, mod := range am.Modules {
			if prev, dup := seenTok[mod.Token]; dup && prev != am.Decl.Name {
				add("CONFORMANCE_DUPLICATE_TOKEN", "blocking", "module "+mod.Token,
					fmt.Sprintf("composed by both %s and %s; tokens are cluster-wide names", prev, am.Decl.Name), "rename one module token")
			}
			seenTok[mod.Token] = am.Decl.Name
			if !mod.IsImage() {
				if mod.SrcDir == "" {
					continue
				}
				if !dirExists(mod.SrcDir) {
					add("CONFORMANCE_MODULE_MISSING", "blocking", "module "+mod.Token, "source "+mod.SrcDir+" is missing", "fix [[compose]].source or restore the module")
				}
			}
		}
		if am.Decl.Cored() && am.File != nil && len(am.Modules) == 0 {
			add("CONFORMANCE_APP_EMPTY", "risk", "app "+am.Decl.Name, "composes no modules", "axon app add module <name>")
		}
		if am.Decl.KindOrDefault() == "project" && am.Decl.Image == "" && am.File != nil {
			if am.File.Project == nil || len(am.File.Project.Build) == 0 {
				add("CONFORMANCE_PROJECT_BUILD_MISSING", "blocking", "app "+am.Decl.Name, "build_type=project requires at least one [[project.build]] step", "add a [[project.build]] block")
			}
		}
	}
	if kb := c.KPIBackend(); strings.Contains(strings.SplitN(kb, ":", 2)[0], "_") {
		add("CONFORMANCE_KPI_BACKEND_NOT_DNS_SAFE", "blocking", "fabric.kpi", fmt.Sprintf("backend host %q has '_'", kb), "use a DNS label (e.g. kpi:7373)")
	}
	if m.HasToken("kpi") && c.Fabric.Clickhouse == nil {
		add("CONFORMANCE_KPI_WITHOUT_CLICKHOUSE", "note", "fabric", "the kpi module runs without [fabric.clickhouse] (Postgres only)", "axon cluster add fabric clickhouse")
	}
	if c.Fabric.Temporal != nil && c.Fabric.Temporal.Shards == 0 {
		add("CONFORMANCE_TEMPORAL_SHARDS_DEFAULT", "note", "fabric.temporal", "shards unset; Temporal defaults to 4 and the value is IMMUTABLE after init", "set shards = 128 before the first deploy")
	}
	for _, k := range m.Unknown {
		if strings.HasPrefix(k, "app.env") || strings.Contains(k, ".env.") {
			continue
		}
		add("CONFORMANCE_UNKNOWN_KEY", "note", "descriptor", fmt.Sprintf("key %q is not understood by axon (ignored)", k), "")
	}
	return ps
}

// ValidateAppFile checks a member descriptor.
func ValidateAppFile(a *AppFile) []Problem {
	var ps []Problem
	switch a.BuildType {
	case "app":
		if a.App.Name == "" {
			ps = append(ps, Problem{ID: "APP_NAME_MISSING", Severity: "blocking", Subject: "[app]", Cause: "[app].name is required (build_type=app)"})
		}
		for _, ce := range a.Compose {
			if ce.Source == "" {
				ps = append(ps, Problem{ID: "APP_COMPOSE_SOURCE_MISSING", Severity: "blocking", Subject: "[[compose]]", Cause: "source is required"})
			}
		}
	case "module":
		if a.Module == nil || a.Module.Token == "" {
			ps = append(ps, Problem{ID: "MODULE_TOKEN_MISSING", Severity: "blocking", Subject: "[module]", Cause: "[module].token is required (build_type=module)"})
		} else if l := a.Module.Language; l != "" && l != "go" && l != "rust" {
			ps = append(ps, Problem{ID: "MODULE_LANGUAGE", Severity: "blocking", Subject: "[module]", Cause: fmt.Sprintf("language %q (go or rust)", l)})
		}
	case "project":
		if a.Project == nil || len(a.Project.Build) == 0 {
			ps = append(ps, Problem{ID: "PROJECT_BUILD_MISSING", Severity: "blocking", Subject: "[project]", Cause: "build_type=project requires at least one [[project.build]] step"})
		}
	case "cluster":
	default:
		ps = append(ps, Problem{ID: "BUILD_TYPE", Severity: "blocking", Subject: "build_type", Cause: fmt.Sprintf("unknown build_type %q (app, module, project, cluster)", a.BuildType)})
	}
	return ps
}

// Blocking counts blocking problems.
func Blocking(ps []Problem) int {
	n := 0
	for _, p := range ps {
		if p.Severity == "blocking" {
			n++
		}
	}
	return n
}
