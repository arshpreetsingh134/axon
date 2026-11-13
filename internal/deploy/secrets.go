package deploy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"axon/internal/dotenv"
	"axon/internal/ui"
)

// RandomSecret returns a URL-safe random string.
func RandomSecret(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[k.Int64()])
	}
	return b.String()
}

// NewSigningKeyPEM generates an ES256 (P-256) private key as PKCS#8 PEM.
func NewSigningKeyPEM() (string, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// WriteSigningKey writes a PEM with mode 0600.
func WriteSigningKey(path, pemText string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(pemText), 0o600)
}

// signingKeyPath resolves [fabric.iam].signing_key relative to the controller dir.
func (s *Session) signingKeyPath() string {
	if iam := s.M.C.Fabric.IAM; iam != nil && iam.SigningKey != "" {
		if filepath.IsAbs(iam.SigningKey) {
			return iam.SigningKey
		}
		return filepath.Join(s.M.Dir, iam.SigningKey)
	}
	return filepath.Join(s.M.Infra, "secrets", "iam_signing_key.pem")
}

// resolveSecrets builds every ${VAR} value workloads need: .env first, then
// values axon derives or generates (persisted to .env / the secrets dir, which
// are gitignored). Nothing secret is written into generated files.
func (s *Session) resolveSecrets(readCluster func(key string) string) error {
	envPath := filepath.Join(s.M.Infra, ".env")
	vals, err := dotenv.Read(envPath)
	if err != nil {
		return fmt.Errorf("%s: %w", envPath, err)
	}
	s.Secrets = vals
	persist := func(k, v, why string) {
		s.Secrets[k] = v
		if s.DryRun {
			return
		}
		if err := dotenv.Set(envPath, k, v); err == nil {
			ui.Note("generated %s into %s (%s)", k, rel(s.M.Dir, envPath), why)
		}
	}
	c := s.M.C
	if _, ok := s.Secrets["POSTGRES_PASSWORD"]; !ok {
		if s.M.L.Runtime() == "mycel" {
			s.Secrets["POSTGRES_PASSWORD"] = "mycel" // what existing mycel volumes were initialized with
			s.pgPassDefaulted = true
		} else {
			persist("POSTGRES_PASSWORD", RandomSecret(24), "no default database credentials")
		}
	}
	if _, ok := s.Secrets["KEYCLOAK_ADMIN_PASSWORD"]; !ok {
		s.Secrets["KEYCLOAK_ADMIN_PASSWORD"] = "admin"
	}
	if kc := c.Fabric.Keycloak; kc != nil {
		if _, ok := s.Secrets["OIDC_CLIENT_SECRET"]; !ok {
			if name, isRef := dotenv.IsPureRef(kc.Secret); isRef {
				if v, ok := s.Secrets[name]; ok {
					s.Secrets["OIDC_CLIENT_SECRET"] = v
				} else {
					persist(name, RandomSecret(32), "OIDC client secret")
					s.Secrets["OIDC_CLIENT_SECRET"] = s.Secrets[name]
				}
			} else {
				s.Secrets["OIDC_CLIENT_SECRET"] = kc.Secret
			}
		}
	}
	if c.Fabric.Clickhouse != nil {
		for _, role := range []string{"WRITER", "READER"} {
			k := strings.ToUpper(s.M.L.Runtime()) + "_CLICKHOUSE_" + role + "_PASSWORD"
			if _, ok := s.Secrets[k]; !ok {
				persist(k, RandomSecret(24), "clickhouse credential")
			}
		}
	}
	if c.IAMApp() != nil {
		if _, ok := s.Secrets["IAM_SIGNING_KEY"]; !ok {
			p := s.signingKeyPath()
			switch b, err := os.ReadFile(p); {
			case err == nil:
				s.Secrets["IAM_SIGNING_KEY"] = string(b)
			default:
				if readCluster != nil {
					if v := readCluster("IAM_SIGNING_KEY"); v != "" {
						s.Secrets["IAM_SIGNING_KEY"] = v
						break
					}
				}
				pemText, err := NewSigningKeyPEM()
				if err != nil {
					return err
				}
				s.Secrets["IAM_SIGNING_KEY"] = pemText
				if !s.DryRun {
					if err := WriteSigningKey(p, pemText); err != nil {
						return err
					}
					ui.Note("generated IAM signing key %s (ES256, 0600)", rel(s.M.Dir, p))
				}
			}
		}
	}
	return nil
}

// Environ renders secrets as KEY=VALUE for a child process (compose interpolation).
func (s *Session) Environ() []string {
	var out []string
	for k, v := range s.Secrets {
		out = append(out, k+"="+v)
	}
	return out
}

// secretManifest renders the app-env Secret (applied via stdin, never written).
func secretManifest(name, ns string, vals map[string]string, labels map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n", name, ns)
	for _, k := range sortedKeys(labels) {
		fmt.Fprintf(&b, "    %s: %q\n", k, labels[k])
	}
	b.WriteString("type: Opaque\ndata:\n")
	for _, k := range sortedKeys(vals) {
		if !validSecretKey(k) {
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(vals[k])))
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func validSecretKey(k string) bool {
	for _, r := range k {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return k != ""
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}
