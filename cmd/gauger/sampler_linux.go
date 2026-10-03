package main

import (
	"github.com/mach4-braai/gauger/internal/agent"
	"github.com/mach4-braai/gauger/internal/procfs"
)

func newSampler() (agent.Sampler, error) {
	return procfs.NewReader(), nil
}
