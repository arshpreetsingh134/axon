package commands

import (
	"axon/internal/deploy"
	"axon/internal/descriptor"
)

type deployOptionsT = deploy.Options
type sessionT = deploy.Session

func openSession(m *descriptor.Model, o deploy.Options) (*deploy.Session, error) {
	return deploy.Open(m, o)
}

func printStatus(st *deploy.Status) { deploy.PrintStatus(st) }
