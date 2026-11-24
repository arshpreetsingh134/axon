package official

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// fabricTemplates are the platform's own objects (front door, workflow
// engine, identity provider, tracing, analytics store, knowledge base), as
// observed in official deploys, with placeholders for every value that
// depends on the cluster, the environment or the release (SPEC.md Appendix C).
//
//go:embed fabric/*.json
var fabricTemplates embed.FS

// FabricVars are the values the fabric templates take.
type FabricVars struct {
	ResourceGroup string // platforms/<env>.outputs.json azure.resource_group
	FrontendName  string // platforms/<env>.outputs.json azure.frontend_name
	DurableClass  string // environments/<env>.toml [storage.durable] class
	DurableSize   string // environments/<env>.toml [storage.durable] size
	Release       map[string]map[string]string
}

// LoadFabricVars reads the fabric values from the platform outputs and the
// environment file; the release tables come from the caller.
func LoadFabricVars(root, env string, release map[string]map[string]string) FabricVars {
	v := FabricVars{Release: release}
	var out struct {
		Azure struct {
			ResourceGroup string `json:"resource_group"`
			FrontendName  string `json:"frontend_name"`
		} `json:"azure"`
	}
	if b, err := os.ReadFile(filepath.Join(root, "platforms", env+".outputs.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	v.ResourceGroup, v.FrontendName = out.Azure.ResourceGroup, out.Azure.FrontendName
	var e struct {
		Storage struct {
			Durable struct {
				Class string `toml:"class"`
				Size  string `toml:"size"`
			} `toml:"durable"`
		} `toml:"storage"`
	}
	_, _ = toml.DecodeFile(filepath.Join(root, "environments", env+".toml"), &e)
	v.DurableClass, v.DurableSize = e.Storage.Durable.Class, e.Storage.Durable.Size
	return v
}

var placeholder = regexp.MustCompile(`\{\{([A-Z_]+)(?::([a-z_]+)\.([A-Za-z0-9_.-]+))?\}\}`)

// fabricNeeds says which declaration a template depends on.
func fabricNeeds(file string) string {
	name := strings.SplitN(strings.TrimSuffix(file, ".json"), "__", 2)[1]
	switch {
	case strings.HasPrefix(name, "clickhouse"):
		return "fabric:clickhouse"
	case strings.HasPrefix(name, "grafana"), strings.HasPrefix(name, "tempo"):
		return "fabric:tracing"
	case strings.HasPrefix(name, "keycloak"):
		return "fabric:keycloak"
	case strings.HasPrefix(name, "temporal"):
		return "fabric:temporal"
	case strings.HasPrefix(name, "helix"):
		return "product:helix"
	case strings.HasPrefix(name, "IAM"), strings.HasPrefix(name, "dbschema-IAM"):
		return "iam"
	}
	return "" // the front door and the database job are always there
}

// RenderFabric renders the fabric objects the descriptor declares. Templates
// whose values are missing are reported in skipped, not guessed.
func RenderFabric(in *RenderInput, v FabricVars) (objs []Object, skipped []string) {
	iamN, iamImage := "", ""
	for _, a := range in.Desc.App {
		if a.Kind == "iam" {
			iamN, iamImage = K8sName(a.Name), in.Images[a.Name]
		}
	}
	vals := map[string]string{"NAMESPACE": in.Namespace, "DOMAIN": in.Domain, "PG_HOST": in.PG.Host, "PG_USER": in.PG.User,
		"RESOURCE_GROUP": v.ResourceGroup, "FRONTEND_NAME": v.FrontendName, "DURABLE_CLASS": v.DurableClass, "DURABLE_SIZE": v.DurableSize,
		"IAM": iamN, "IAM_IMAGE": iamImage}
	files, _ := fabricTemplates.ReadDir("fabric")
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	for _, f := range names {
		switch need := fabricNeeds(f); {
		case strings.HasPrefix(need, "fabric:") && !in.Desc.HasFabric(strings.TrimPrefix(need, "fabric:")):
			continue
		case need == "product:helix":
			if _, ok := in.Desc.Product["helix"]; !ok {
				continue
			}
		case need == "iam" && iamN == "":
			continue
		}
		b, _ := fabricTemplates.ReadFile("fabric/" + f)
		var missing []string
		text := placeholder.ReplaceAllStringFunc(string(b), func(m string) string {
			p := placeholder.FindStringSubmatch(m)
			var val string
			if p[1] == "IMAGE" {
				val = v.Release[p[2]][p[3]]
			} else {
				val = vals[p[1]]
			}
			if val == "" {
				missing = append(missing, strings.Trim(m, "{}"))
			}
			return strings.ReplaceAll(val, `"`, `\"`)
		})
		if len(missing) > 0 {
			skipped = append(skipped, fmt.Sprintf("%s (missing %s)", strings.Replace(strings.TrimSuffix(f, ".json"), "__", "/", 1), strings.Join(missing, ", ")))
			continue
		}
		var o Object
		if err := json.Unmarshal([]byte(text), &o); err != nil {
			skipped = append(skipped, f+": "+err.Error())
			continue
		}
		objs = append(objs, o)
	}
	return objs, skipped
}

//go:embed sql/*.sql
var sqlFiles embed.FS

// Databases lists the logical databases an official deploy ensures, in order:
// every [[app]] db in descriptor order, then the products' (the console), the
// identity provider's and the knowledge base's.
func (d *Descriptor) Databases() []string {
	var out []string
	seen := map[string]bool{}
	add := func(db string) {
		if db != "" && !seen[db] {
			seen[db] = true
			out = append(out, db)
		}
	}
	for _, a := range d.App {
		add(a.DB)
	}
	if _, ok := d.Product["clc"]; ok {
		add("console")
	}
	if d.HasFabric("keycloak") {
		add("keycloak")
	}
	if _, ok := d.Product["helix"]; ok {
		add("helix")
	}
	return out
}

// RenderDatabases renders the ConfigMap of the job that creates every logical
// database and installs the audit extension in each.
func RenderDatabases(in *RenderInput) Object {
	header, _ := sqlFiles.ReadFile("sql/databases-header.sql")
	block, _ := sqlFiles.ReadFile("sql/audit-block.sql")
	probe, _ := sqlFiles.ReadFile("sql/audit-probe.sql")
	var b strings.Builder
	b.Write(header)
	dbs := in.Desc.Databases()
	for _, db := range dbs {
		fmt.Fprintf(&b, "SELECT 'CREATE DATABASE %s' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%s')\\gexec\n", db, db)
	}
	b.WriteString("-- Install pgAudit in every managed database.\n")
	for _, db := range append([]string{"postgres"}, dbs...) {
		b.WriteString("\\connect " + db + "\n")
		b.Write(block)
	}
	return Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"annotations": map[string]any{}, "name": "ensure-databases", "namespace": in.Namespace},
		"data":     map[string]any{"01-databases.sql": b.String(), "02-audit-probe.sql": string(probe)}}
}
