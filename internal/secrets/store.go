// Package secrets is axon's secret store: named values kept in a Kubernetes Secret
// per project and environment, in a dedicated namespace (so tearing down or
// deleting an app namespace never loses them).
//
// Values exist only in memory and in the cluster. They are never written to a file,
// passed as a command-line argument (the Secret is sent to kubectl on stdin), or
// printed.
package secrets

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"axon/internal/sh"
)

// Namespace holds every project's store.
const Namespace = "axon-secrets"

var nameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidName reports whether s is usable as an environment variable name.
func ValidName(s string) bool { return nameRE.MatchString(s) && len(s) <= 120 }

// Store is one project+environment's secrets.
type Store struct {
	Context string // kube context ("" = current)
	Name    string // Secret name: <project>.<env>
}

// New returns the store for a project and environment.
func New(kubeContext, project, env string) *Store {
	return &Store{Context: kubeContext, Name: strings.ToLower(project) + "." + strings.ToLower(env)}
}

// Meta describes one stored secret (never its value).
type Meta struct {
	Name    string `json:"name"`
	Updated string `json:"updated"`
}

func (s *Store) kubectl(stdin string, args ...string) (*sh.Result, error) {
	base := []string{}
	if s.Context != "" {
		base = append(base, "--context", s.Context)
	}
	return sh.Run(sh.Cmd{Name: "kubectl", Args: append(base, args...), Stdin: stdin, Timeout: 30 * time.Second})
}

// Available reports whether the cluster that holds the store answers.
func (s *Store) Available() bool {
	if !sh.Has("kubectl") {
		return false
	}
	_, err := s.kubectl("", "get", "ns", "--request-timeout=5s", "-o", "name")
	return err == nil
}

type secretDoc struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

const metaAnnotation = "axon.dev/updated"

// load returns values and update times (missing store → empty).
func (s *Store) load() (map[string]string, map[string]string, bool, error) {
	r, err := s.kubectl("", "-n", Namespace, "get", "secret", s.Name, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, nil, false, fmt.Errorf("read the secret store: %w", err)
	}
	vals, updated := map[string]string{}, map[string]string{}
	if strings.TrimSpace(r.Stdout) == "" {
		return vals, updated, false, nil
	}
	var d secretDoc
	if err := json.Unmarshal([]byte(r.Stdout), &d); err != nil {
		return nil, nil, false, err
	}
	for k, v := range d.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, nil, false, fmt.Errorf("secret %s is not valid base64", k)
		}
		vals[k] = string(b)
	}
	if m := d.Metadata.Annotations[metaAnnotation]; m != "" {
		_ = json.Unmarshal([]byte(m), &updated)
	}
	return vals, updated, true, nil
}

// Values returns every stored value (in memory only).
func (s *Store) Values() (map[string]string, error) {
	v, _, _, err := s.load()
	return v, err
}

// List returns names and update times, sorted.
func (s *Store) List() ([]Meta, error) {
	vals, updated, _, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for k := range vals {
		out = append(out, Meta{Name: k, Updated: updated[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Set stores or replaces values.
func (s *Store) Set(kv map[string]string) error {
	for k := range kv {
		if !ValidName(k) {
			return fmt.Errorf("%q is not a valid secret name (use UPPER_SNAKE_CASE, like an environment variable)", k)
		}
	}
	vals, updated, exists, err := s.load()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range kv {
		vals[k] = v
		updated[k] = now
	}
	return s.write(vals, updated, exists)
}

// Delete removes names; it reports which were absent.
func (s *Store) Delete(names ...string) ([]string, error) {
	vals, updated, exists, err := s.load()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, n := range names {
		if _, ok := vals[n]; !ok {
			missing = append(missing, n)
			continue
		}
		delete(vals, n)
		delete(updated, n)
	}
	if len(missing) == len(names) {
		return missing, nil
	}
	return missing, s.write(vals, updated, exists)
}

func (s *Store) ensureNamespace() error {
	ns := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    app.kubernetes.io/managed-by: axon\n", Namespace)
	_, err := s.kubectl(ns, "apply", "-f", "-")
	return err
}

// write replaces the whole Secret. create/replace (not apply) so values are never
// copied into a last-applied-configuration annotation.
func (s *Store) write(vals, updated map[string]string, exists bool) error {
	if err := s.ensureNamespace(); err != nil {
		return fmt.Errorf("create namespace %s: %w", Namespace, err)
	}
	meta, _ := json.Marshal(updated)
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\ntype: Opaque\nmetadata:\n  name: %s\n  namespace: %s\n", s.Name, Namespace)
	fmt.Fprintf(&b, "  labels:\n    axon.dev/store: secrets\n  annotations:\n    %s: %q\n", metaAnnotation, string(meta))
	b.WriteString("data:\n")
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(vals[k])))
	}
	verb := "create"
	if exists {
		verb = "replace"
	}
	if _, err := s.kubectl(b.String(), verb, "-f", "-"); err != nil {
		return fmt.Errorf("write the secret store: %w", err)
	}
	return nil
}
