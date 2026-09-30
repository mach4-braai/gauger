// Package metrics turns /proc samples into OTLP metrics tagged with the job's identity.
package metrics

import "strings"

// Identity attribute keys, shared with gauger-server. docs/contract.md lists them.
const (
	KeyRunID      = "cicd.pipeline.run.id"
	KeyRunAttempt = "github.run_attempt"
	KeyCheckRunID = "cicd.pipeline.task.run.id"
	KeyOwner      = "vcs.owner.name"
	KeyRepository = "vcs.repository.name"
	KeyWorkflow   = "cicd.pipeline.name"
	KeyJob        = "cicd.pipeline.task.name"
	KeyRunnerName = "cicd.worker.name"
	KeyScope      = "gauger.metrics.scope"
)

// Attribute is one string attribute. A slice keeps the order stable on the wire.
type Attribute struct {
	Key, Value string
}

// Identity names the job the runner is working on.
type Identity struct {
	RunID      string
	RunAttempt string
	CheckRunID string
	Repository string
	Workflow   string
	Job        string
	RunnerName string
}

// IdentityFromEnv reads the identity from the runner's environment. checkRunID
// comes from the action input, because no environment variable carries it.
func IdentityFromEnv(getenv func(string) string, checkRunID string) Identity {
	return Identity{
		RunID:      getenv("GITHUB_RUN_ID"),
		RunAttempt: getenv("GITHUB_RUN_ATTEMPT"),
		CheckRunID: checkRunID,
		Repository: getenv("GITHUB_REPOSITORY"),
		Workflow:   getenv("GITHUB_WORKFLOW"),
		Job:        getenv("GITHUB_JOB"),
		RunnerName: getenv("RUNNER_NAME"),
	}
}

// Attributes returns the identity as OTel attributes. Empty values are left
// out, so a job without a check run ID is matched on its runner name.
func (id Identity) Attributes() []Attribute {
	owner, repo, _ := strings.Cut(id.Repository, "/")
	all := []Attribute{
		{KeyRunID, id.RunID},
		{KeyRunAttempt, id.RunAttempt},
		{KeyCheckRunID, id.CheckRunID},
		{KeyOwner, owner},
		{KeyRepository, repo},
		{KeyWorkflow, id.Workflow},
		{KeyJob, id.Job},
		{KeyRunnerName, id.RunnerName},
		{KeyScope, "runner"},
	}
	attrs := all[:0]
	for _, a := range all {
		if a.Value != "" {
			attrs = append(attrs, a)
		}
	}
	return attrs
}
