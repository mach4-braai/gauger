package main

import (
	"github.com/mach4-braai/gauger/internal/agent"
	"github.com/mach4-braai/gauger/internal/macos"
)

func newSampler() (agent.Sampler, error) {
	return macos.NewReader()
}
