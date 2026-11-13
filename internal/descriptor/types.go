// Package descriptor models the TOML descriptor (axonfile / mycelfile) for every
// build_type: cluster, app, module and project.
package descriptor

import (
	"fmt"
	"sort"
	"strings"
)

// Volume is an inline volume table: { name, kind = "scratch"|"durable", mount }.
type Volume struct {
	Name  string `toml:"name"`
	Kind  string `toml:"kind"`
	Mount string `toml:"mount"`
}

// Postgres is [fabric.postgres] (required for a cluster).
type Postgres struct {
	Image  string  `toml:"image"`
	Port   int     `toml:"port"`
	User   string  `toml:"user"` // axon extension; default "axon" (compat: "mycel")
	Volume *Volume `toml:"volume"`
	// MaxConnections tunes the server (axon extension; default 300).
	MaxConnections int `toml:"max_connections"`
}

// Temporal is [fabric.temporal].
type Temporal struct {
	Image     string `toml:"image"`
	Shards    int    `toml:"shards"`
	Port      int    `toml:"port"`
	UIPort    int    `toml:"ui_port"`
	UIImage   string `toml:"ui_image"`
	Namespace string `toml:"namespace"`
}

// Keycloak is [fabric.keycloak].
type Keycloak struct {
	Image      string `toml:"image"`
	Realm      string `toml:"realm"`
	Client     string `toml:"client"`
	Secret     string `toml:"secret"`
	AdminEmail string `toml:"admin_email"`
}

// Traefik is [fabric.traefik].
type Traefik struct {
	HTTP  int    `toml:"http"`
	Image string `toml:"image"`
}

// Tracing is [fabric.tracing].
type Tracing struct {
	TempoImage    string  `toml:"tempo_image"`
	GrafanaImage  string  `toml:"grafana_image"`
	GrafanaPort   int     `toml:"grafana_port"`
	TempoVolume   *Volume `toml:"tempo_volume"`
	GrafanaVolume *Volume `toml:"grafana_volume"`
}

// Clickhouse is [fabric.clickhouse].
type Clickhouse struct {
	Image    string  `toml:"image"`
	HTTPPort int     `toml:"http_port"`
	Database string  `toml:"database"`
	Volume   *Volume `toml:"volume"`
}

// KPI is [fabric.kpi].
type KPI struct {
	Backend string `toml:"backend"`
}

// IAM is [fabric.iam], the cluster identity contract.
type IAM struct {
	Issuer     string `toml:"issuer"`
	Audience   string `toml:"audience"`
	Backend    string `toml:"backend"`
	SigningKey string `toml:"signing_key"`
}

// Console is [fabric.console] (change-ledger push target; axon keeps a local spool only).
type Console struct {
	PushActor    string `toml:"push_actor"`
	PushPassword string `toml:"push_password"`
	License      string `toml:"license"`
}

// Fabric is the [fabric.*] set.
type Fabric struct {
	Postgres   *Postgres   `toml:"postgres"`
	Temporal   *Temporal   `toml:"temporal"`
	Keycloak   *Keycloak   `toml:"keycloak"`
	Traefik    *Traefik    `toml:"traefik"`
	Tracing    *Tracing    `toml:"tracing"`
	Clickhouse *Clickhouse `toml:"clickhouse"`
	KPI        *KPI        `toml:"kpi"`
	IAM        *IAM        `toml:"iam"`
	Console    *Console    `toml:"console"`
}

// Product is [product.<name>].
type Product struct {
	Image string `toml:"image"`
	Host  string `toml:"host"`
	App   string `toml:"app"`
	KB    string `toml:"kb"`
	Mode  string `toml:"mode"`
}

// EnvMap is a string map that accepts any scalar TOML values.
type EnvMap map[string]string

// UnmarshalTOML implements toml.Unmarshaler.
func (e *EnvMap) UnmarshalTOML(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("env must be a table")
	}
	out := EnvMap{}
	for k, val := range m {
		switch t := val.(type) {
		case string:
			out[k] = t
		case bool, int64, float64:
			out[k] = fmt.Sprint(t)
		default:
			return fmt.Errorf("env %s: unsupported value %v", k, val)
		}
	}
	*e = out
	return nil
}

// Keys returns sorted keys.
func (e EnvMap) Keys() []string {
	var ks []string
	for k := range e {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// App is one [[app]] member of a cluster.
type App struct {
	Name        string `toml:"name"`
	Kind        string `toml:"kind"` // module (default) | iam | project
	Host        string `toml:"host"`
	DB          string `toml:"db"`
	Port        int    `toml:"port"`
	Temporal    string `toml:"temporal"`
	Image       string `toml:"image"`
	Command     string `toml:"command"`
	Mount       string `toml:"mount"`
	ExternalURL string `toml:"external_url"`
	Env         EnvMap `toml:"env"`
	// axon extensions
	Replicas int  `toml:"replicas"`
	Disabled bool `toml:"disabled"`
}

// KindOrDefault returns the app kind ("module" when unset).
func (a *App) KindOrDefault() string {
	if a.Kind == "" {
		return "module"
	}
	return a.Kind
}

// Cored reports whether the app is a core host plus modules (module|iam).
func (a *App) Cored() bool { return a.KindOrDefault() != "project" }

// Hooks are lifecycle commands (axon extension).
type Hooks struct {
	PreGenerate []string `toml:"pre_generate"`
	PreBuild    []string `toml:"pre_build"`
	PostBuild   []string `toml:"post_build"`
	PostFabric  []string `toml:"post_fabric"`
	PostDeploy  []string `toml:"post_deploy"`
	PreDown     []string `toml:"pre_down"`
	PostDown    []string `toml:"post_down"`
}

// Merge appends other's hooks after h's.
func (h Hooks) Merge(o Hooks) Hooks {
	return Hooks{
		PreGenerate: append(append([]string{}, h.PreGenerate...), o.PreGenerate...),
		PreBuild:    append(append([]string{}, h.PreBuild...), o.PreBuild...),
		PostBuild:   append(append([]string{}, h.PostBuild...), o.PostBuild...),
		PostFabric:  append(append([]string{}, h.PostFabric...), o.PostFabric...),
		PostDeploy:  append(append([]string{}, h.PostDeploy...), o.PostDeploy...),
		PreDown:     append(append([]string{}, h.PreDown...), o.PreDown...),
		PostDown:    append(append([]string{}, h.PostDown...), o.PostDown...),
	}
}

// Stage returns the hooks for a named stage.
func (h Hooks) Stage(name string) []string {
	switch name {
	case "pre_generate":
		return h.PreGenerate
	case "pre_build":
		return h.PreBuild
	case "post_build":
		return h.PostBuild
	case "post_fabric":
		return h.PostFabric
	case "post_deploy":
		return h.PostDeploy
	case "pre_down":
		return h.PreDown
	case "post_down":
		return h.PostDown
	}
	return nil
}

// DeployDefaults is [deploy] (axon extension).
type DeployDefaults struct {
	Env      string   `toml:"env"`
	Target   string   `toml:"target"`
	Jobs     int      `toml:"jobs"`
	Timeout  string   `toml:"timeout"`
	Products bool     `toml:"products"` // deploy licensed [product.*] images
	Skip     []string `toml:"skip"`     // app names to leave out
	Context  string   `toml:"context"`  // default kube context
	// SkipFabric leaves declared fabric services out (e.g. a heavy store locally).
	SkipFabric []string `toml:"skip_fabric"`
	// RuntimePrefix keeps runtime names (database role, Kubernetes objects,
	// ClickHouse users) under another prefix, e.g. "mycel" after `axon migrate`.
	RuntimePrefix string `toml:"runtime_prefix"`
	// Postgres is the default k8s Postgres mode (in-cluster | docker | external:<h>:<p>)
	// when the env file does not set one.
	Postgres string `toml:"postgres"`
}

// CIConfig is [ci] (axon extension): how `axon ci init` generates the pipeline.
type CIConfig struct {
	Provider    string   `toml:"provider"`     // github (default)
	Branches    []string `toml:"branches"`     // push branches (default ["main"])
	Runner      string   `toml:"runner"`       // runs-on (default ubuntu-latest)
	E2E         *bool    `toml:"e2e"`          // deploy + journeys on the runner (default true)
	AppTests    *bool    `toml:"app_tests"`    // axon app test per source app (default true)
	Apps        []string `toml:"apps"`         // e2e subset (default: every app)
	Env         string   `toml:"env"`          // environment used in CI (default ci)
	Hooks       *bool    `toml:"hooks"`        // run lifecycle hooks in CI (default true)
	Timeout     string   `toml:"timeout"`      // per-step deploy timeout (default 20m)
	AxonVersion string   `toml:"axon_version"` // axon release to install (default latest)
	AxonRepo    string   `toml:"axon_repo"`    // where axon releases live
	// Checkouts maps a workspace-relative path to "owner/repo[@ref]": every repository the
	// project is assembled from, placed where it lives on a developer machine.
	Checkouts map[string]string `toml:"checkouts"`
	// Links maps a workspace-relative symlink to its workspace-relative target
	// (member apps that are links into a checkout).
	Links map[string]string `toml:"links"`
}

// ReleaseConfig is [release] (axon extension).
type ReleaseConfig struct {
	// Registry is where `release create --push` publishes images, e.g. ghcr.io/owner.
	Registry string `toml:"registry"`
	// Platforms lists the image platforms to build, e.g. ["linux/amd64", "linux/arm64"].
	// More than one produces a multi-architecture image (docker buildx); empty = the host's.
	Platforms []string `toml:"platforms"`
}

// Cluster is a build_type = "cluster" descriptor.
type Cluster struct {
	BuildType string             `toml:"build_type"`
	Name      string             `toml:"name"`
	Network   string             `toml:"network"`
	Domain    string             `toml:"domain"`
	CoreImage string             `toml:"core_image"`
	Fabric    Fabric             `toml:"fabric"`
	Product   map[string]Product `toml:"product"`
	Apps      []App              `toml:"app"`
	// axon extensions (also accepted from axon.toml)
	Hooks   Hooks             `toml:"hooks"`
	Images  map[string]string `toml:"images"`
	Deploy  DeployDefaults    `toml:"deploy"`
	CI      CIConfig          `toml:"ci"`
	Release ReleaseConfig     `toml:"release"`
}

// App returns the [[app]] named name.
func (c *Cluster) App(name string) *App {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i]
		}
	}
	return nil
}

// HTTPPort is the front-door host port.
func (c *Cluster) HTTPPort() int {
	if c.Fabric.Traefik != nil && c.Fabric.Traefik.HTTP > 0 {
		return c.Fabric.Traefik.HTTP
	}
	return 8090
}

// DomainOrDefault returns the routing domain.
func (c *Cluster) DomainOrDefault() string {
	if c.Domain == "" {
		return "localhost"
	}
	return c.Domain
}

// NetworkOrDefault returns the docker network.
func (c *Cluster) NetworkOrDefault() string {
	if c.Network == "" {
		return c.Name + "-net"
	}
	return c.Network
}

// FrontDoor returns http://<host>.<domain>:<port>.
func (c *Cluster) FrontDoor(host string) string {
	return fmt.Sprintf("http://%s.%s:%d", host, c.DomainOrDefault(), c.HTTPPort())
}

// KPIBackend returns the analytics contract address.
func (c *Cluster) KPIBackend() string {
	if c.Fabric.KPI != nil && c.Fabric.KPI.Backend != "" {
		return c.Fabric.KPI.Backend
	}
	return "kpi:7373"
}

// IAMToken returns the module token of the identity backend ("mycel_iam" from "mycel_iam:7373").
func (c *Cluster) IAMToken() string {
	if c.Fabric.IAM == nil || c.Fabric.IAM.Backend == "" {
		return ""
	}
	return strings.SplitN(c.Fabric.IAM.Backend, ":", 2)[0]
}

// IAMApp returns the kind="iam" app, if any.
func (c *Cluster) IAMApp() *App {
	for i := range c.Apps {
		if c.Apps[i].Kind == "iam" {
			return &c.Apps[i]
		}
	}
	return nil
}

// ComposeEntry is one [[compose]] module of an app.
type ComposeEntry struct {
	Source string `toml:"source"`
	Image  string `toml:"image"`
	Token  string `toml:"token"`
	DB     *bool  `toml:"db"`
	Ext    string `toml:"ext"`
}

// ModuleSpec is [module] of a build_type = "module" descriptor.
type ModuleSpec struct {
	Token    string `toml:"token"`
	Language string `toml:"language"`
	Element  string `toml:"element"`
	UIEntry  string `toml:"ui_entry"`
	Label    string `toml:"label"`
	Backend  string `toml:"backend"`
}

// BuildStep is [[project.build]].
type BuildStep struct {
	Name    string `toml:"name"`
	Command string `toml:"command"`
}

// ProjectSpec is [project].
type ProjectSpec struct {
	Dockerfile string      `toml:"dockerfile"`
	Build      []BuildStep `toml:"build"`
	Run        *struct {
		Command string `toml:"command"`
	} `toml:"run"`
}

// AppFile is a member descriptor (build_type app | module | project).
type AppFile struct {
	BuildType string `toml:"build_type"`
	App       struct {
		Name              string `toml:"name"`
		Title             string `toml:"title"`
		SearchPlaceholder string `toml:"search_placeholder"`
	} `toml:"app"`
	Home struct {
		Title string `toml:"title"`
		Body  string `toml:"body"`
	} `toml:"home"`
	Compose []ComposeEntry `toml:"compose"`
	Shell   *struct {
		Source string `toml:"source"`
	} `toml:"shell"`
	Module  *ModuleSpec  `toml:"module"`
	Project *ProjectSpec `toml:"project"`
}
