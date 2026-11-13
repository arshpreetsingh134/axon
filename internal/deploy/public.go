package deploy

// K8sPreflightPublic resolves and checks the kube context.
func (s *Session) K8sPreflightPublic() error { return s.k8sPreflight() }

// PreparePublic resolves secrets and the Postgres endpoint without changing
// anything (read-only verbs: status, logs, diagnose).
func (s *Session) PreparePublic() error {
	saved := s.DryRun
	s.DryRun = true
	defer func() { s.DryRun = saved }()
	if err := s.resolveSecrets(nil); err != nil {
		return err
	}
	return s.preparePostgres()
}
