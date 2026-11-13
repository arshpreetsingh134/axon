package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"axon/internal/descriptor"
	"axon/internal/dotenv"
	"axon/internal/gen"
	"axon/internal/sh"
	"axon/internal/ui"
)

// preparePostgres decides how workloads reach Postgres.
//
//	docker target: the fabric's own postgres service.
//	k8s target:    env `postgres` = in-cluster | docker | external:<host>:<port>;
//	               default: "docker" when a <cluster>-postgres container exists, else in-cluster.
func (s *Session) preparePostgres() error {
	user := gen.PGUser(s.M)
	if s.Target == "docker" {
		s.PGMode = "compose"
		s.PG = gen.PG{Host: "postgres", Port: 5432, User: user, Password: "${POSTGRES_PASSWORD}"}
		return nil
	}
	mode := firstNonEmpty(s.Env.Postgres, s.M.C.Deploy.Postgres)
	s.PGContainer = descriptor.Slug(s.M.C.Name) + "-postgres"
	if mode == "" {
		if containerExists(s.PGContainer) {
			mode = "docker"
		} else {
			mode = "in-cluster"
		}
	}
	switch {
	case mode == "in-cluster":
		s.PGMode = mode
		s.PG = gen.PG{Host: "postgres", Port: 5432, User: user, Password: "${POSTGRES_PASSWORD}"}
	case mode == "docker":
		s.PGMode = mode
		if err := s.ensurePGContainer(user); err != nil {
			return err
		}
	case strings.HasPrefix(mode, "external:"):
		s.PGMode = "external"
		hp := strings.TrimPrefix(mode, "external:")
		host, port := hp, 5432
		if i := strings.LastIndex(hp, ":"); i > 0 {
			host = hp[:i]
			port, _ = strconv.Atoi(hp[i+1:])
		}
		s.PG = gen.PG{Host: host, Port: port, User: user, Password: "${POSTGRES_PASSWORD}"}
	default:
		return fmt.Errorf("environments/%s.toml postgres = %q (in-cluster, docker or external:<host>:<port>)", s.Env.Name, mode)
	}
	return nil
}

func containerExists(name string) bool {
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"container", "inspect", name}, Timeout: 20 * time.Second})
	return err == nil
}

// ensurePGContainer starts (or adopts) the machine-local Postgres container and
// reads its real credentials from the container itself.
func (s *Session) ensurePGContainer(defaultUser string) error {
	name := s.PGContainer
	pg := s.M.C.Fabric.Postgres
	if pg == nil {
		pg = &descriptor.Postgres{}
	}
	if !containerExists(name) {
		// a brand-new data volume gets a real password, not the compat default
		if s.pgPassDefaulted && !volumeExists(name+"-data") && !s.DryRun {
			v := RandomSecret(32)
			if err := dotenv.Set(filepath.Join(s.M.Infra, ".env"), "POSTGRES_PASSWORD", v); err != nil {
				return err
			}
			s.Secrets["POSTGRES_PASSWORD"] = v
			s.pgPassDefaulted = false
			ui.Note("generated POSTGRES_PASSWORD into %s (new database)", rel(s.M.Dir, filepath.Join(s.M.Infra, ".env")))
		}
		if s.DryRun {
			ui.Sub("would start postgres container %s on 127.0.0.1:%d", name, pg.Port)
		} else {
			ui.Sub("starting postgres container %s", name)
			img := s.M.SubstImage(or(pg.Image, gen.ImgPostgres))
			args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped",
				"--label", "axon.dev/cluster=" + s.M.C.Name,
				"-e", "POSTGRES_USER=" + defaultUser, "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=postgres",
				"-v", name + "-data:/var/lib/postgresql/data"}
			if pg.Port > 0 {
				args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:5432", pg.Port))
			}
			maxConn := pg.MaxConnections
			if maxConn == 0 {
				maxConn = 300
			}
			args = append(args, img, "-c", fmt.Sprintf("max_connections=%d", maxConn))
			if _, err := sh.Run(sh.Cmd{Name: "docker", Args: args, Env: []string{"POSTGRES_PASSWORD=" + s.Secrets["POSTGRES_PASSWORD"]}}); err != nil {
				return err
			}
		}
	} else if !s.DryRun {
		if out, _ := sh.Output("docker", "inspect", "-f", "{{.State.Running}}", name); out != "true" {
			ui.Sub("starting postgres container %s", name)
			if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"start", name}}); err != nil {
				return err
			}
		}
	}
	user, pass := defaultUser, s.Secrets["POSTGRES_PASSWORD"]
	if out, err := sh.Output("docker", "inspect", "-f", "{{json .Config.Env}}", name); err == nil {
		var env []string
		if json.Unmarshal([]byte(out), &env) == nil {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, "POSTGRES_USER="); ok {
					user = v
				}
				if v, ok := strings.CutPrefix(kv, "POSTGRES_PASSWORD="); ok {
					pass = v
				}
			}
		}
	}
	s.Secrets["POSTGRES_PASSWORD"] = pass
	host, port := "host.docker.internal", pg.Port
	if isOrbStack(s.KubeContext) {
		host, port = name+".orb.local", 5432
	}
	if port == 0 {
		port = 5432
	}
	s.PG = gen.PG{Host: host, Port: port, User: user, Password: "${POSTGRES_PASSWORD}"}
	if !s.DryRun {
		waitFor(60*time.Second, func() bool {
			_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"exec", name, "pg_isready", "-U", user}})
			return err == nil
		})
	}
	return nil
}

func volumeExists(name string) bool {
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"volume", "inspect", name}, Timeout: 20 * time.Second})
	return err == nil
}

func isOrbStack(ctx string) bool {
	if ctx == "orbstack" {
		return true
	}
	out, _ := sh.Output("docker", "info", "--format", "{{.OperatingSystem}}")
	return strings.Contains(strings.ToLower(out), "orbstack")
}

func waitFor(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// psql runs SQL against db on whichever Postgres this session uses.
func (s *Session) psql(db, sql string) (string, error) {
	user := s.PG.User
	args := []string{"psql", "-v", "ON_ERROR_STOP=1", "-qtA", "-U", user, "-d", db}
	var c sh.Cmd
	switch s.PGMode {
	case "compose":
		c = sh.Cmd{Name: "docker", Args: append([]string{"compose", "-p", s.fabricProject(), "-f", s.fabricCompose(), "exec", "-T", "postgres"}, args...), Env: s.Environ()}
	case "docker":
		c = sh.Cmd{Name: "docker", Args: append([]string{"exec", "-i", s.PGContainer}, args...)}
	case "in-cluster":
		c = sh.Cmd{Name: "kubectl", Args: append(s.kargs("exec", "-i", "statefulset/postgres", "--"), args...)}
	default:
		return "", fmt.Errorf("postgres is external (%s:%d); axon cannot run SQL there — create databases %s yourself",
			s.PG.Host, s.PG.Port, strings.Join(gen.Databases(s.M), ", "))
	}
	c.Stdin = sql
	c.Timeout = 2 * time.Minute
	r, err := sh.Run(c)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

// ensureDatabases creates every logical database that is missing (idempotent).
func (s *Session) ensureDatabases() error {
	dbs := gen.Databases(s.M)
	if s.DryRun {
		ui.Sub("would ensure databases: %s", strings.Join(dbs, ", "))
		return nil
	}
	if s.PGMode == "in-cluster" {
		if _, err := sh.Run(sh.Cmd{Name: "kubectl", Args: s.kargs("rollout", "status", "statefulset/postgres", "--timeout=300s")}); err != nil {
			return fmt.Errorf("postgres did not become ready: %w", err)
		}
	}
	if !waitFor(90*time.Second, func() bool { _, err := s.psql("postgres", "SELECT 1"); return err == nil }) {
		return fmt.Errorf("postgres is not accepting connections")
	}
	if _, err := s.psql("postgres", gen.EnsureDatabasesSQL(dbs)); err != nil {
		if s.PGMode == "external" {
			ui.Warn("%v", err)
			return nil
		}
		return fmt.Errorf("ensure databases: %w", err)
	}
	ui.OK("databases ensured: %s", strings.Join(dbs, ", "))
	return nil
}

// provisionKeycloak waits for Keycloak behind the front door, then runs the
// cluster's provision-keycloak.sh with the derived redirect URIs.
func (s *Session) provisionKeycloak() error {
	kc := s.M.C.Fabric.Keycloak
	script := filepath.Join(s.M.Infra, "scripts", "provision-keycloak.sh")
	if kc == nil {
		return nil
	}
	if _, err := os.Stat(script); err != nil {
		ui.Note("no %s; skipping Keycloak provisioning", rel(s.M.Dir, script))
		return nil
	}
	if s.DryRun {
		ui.Sub("would provision Keycloak realm %s via %s", kc.Realm, rel(s.M.Dir, script))
		return nil
	}
	host := "keycloak." + s.M.C.DomainOrDefault()
	ui.Sub("waiting for keycloak at %s", s.M.C.FrontDoor("keycloak"))
	if !waitFor(5*time.Minute, func() bool {
		return probe(s.M.C.HTTPPort(), host, "/realms/master/.well-known/openid-configuration") == 200
	}) {
		return fmt.Errorf("keycloak never answered at %s", s.M.C.FrontDoor("keycloak"))
	}
	var hosts []string
	for _, h := range gen.Hosts(s.M, s.M.C.Deploy.Products) {
		if !strings.HasPrefix(h, "keycloak.") && !strings.HasPrefix(h, "grafana.") && !strings.HasPrefix(h, "temporal.") {
			hosts = append(hosts, fmt.Sprintf("http://%s:%d/*", h, s.M.C.HTTPPort()))
		}
	}
	redirect, _ := json.Marshal(hosts)
	env := append(s.Environ(),
		"KC="+s.M.C.FrontDoor("keycloak"), "REALM="+kc.Realm, "CLIENT="+kc.Client,
		"CLIENT_SECRET="+s.Secrets["OIDC_CLIENT_SECRET"], "REDIRECT_URIS="+string(redirect),
		"POST_LOGOUT_URIS="+strings.Join(hosts, "##"), "KEYCLOAK_ADMIN_PASSWORD="+s.Secrets["KEYCLOAK_ADMIN_PASSWORD"])
	ui.Step("provision keycloak (realm %s)", kc.Realm)
	if _, err := sh.Run(sh.Cmd{Name: "bash", Args: []string{script}, Dir: s.M.Dir, Env: env, Stream: true, Prefix: "  │ ", Timeout: 5 * time.Minute}); err != nil {
		return fmt.Errorf("provision-keycloak.sh: %w", err)
	}
	return nil
}

// seedIAM runs scripts/seed-iam.sql once the IAM module has created its schema.
func (s *Session) seedIAM() error {
	iam := s.M.C.IAMApp()
	tok := s.M.C.IAMToken()
	script := filepath.Join(s.M.Infra, "scripts", "seed-iam.sql")
	if iam == nil || iam.DB == "" || tok == "" {
		return nil
	}
	b, err := os.ReadFile(script)
	if err != nil {
		return nil
	}
	if s.PGMode == "external" {
		ui.Note("postgres is external; run %s yourself", rel(s.M.Dir, script))
		return nil
	}
	if s.DryRun {
		ui.Sub("would seed IAM from %s", rel(s.M.Dir, script))
		return nil
	}
	ui.Sub("waiting for the %s schema", tok)
	q := fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s'", tok)
	if !waitFor(5*time.Minute, func() bool {
		out, err := s.psql(iam.DB, q)
		n, _ := strconv.Atoi(out)
		return err == nil && n > 0
	}) {
		return fmt.Errorf("the %s module never created its schema in database %s (check its logs)", tok, iam.DB)
	}
	if _, err := s.psql(iam.DB, string(b)); err != nil {
		return fmt.Errorf("seed-iam.sql: %w", err)
	}
	ui.OK("IAM seeded")
	return nil
}
