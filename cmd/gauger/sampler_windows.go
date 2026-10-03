package main

import (
	"github.com/mach4-braai/gauger/internal/agent"
	"github.com/mach4-braai/gauger/internal/winmetrics"
)

func newSampler() (agent.Sampler, error) {
	return winmetrics.NewReader()
}
