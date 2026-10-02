// Package metrics turns /proc samples into OTLP metrics tagged with the job's identity.
package metrics

// Identity attribute keys, shared with gauger-server. docs/contract.md lists them.
const (
	KeyRunID      = "github.run_id"
	KeyRunAttempt = "github.run_attempt"
	KeyCheckRunID = "github.check_run_id"
	KeyRepository = "github.repository"
	KeyWorkflow   = "github.workflow"
	KeyJob        = "github.job"
	KeyRunnerName = "runner.name"
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
	all := []Attribute{
		{KeyRunID, id.RunID},
		{KeyRunAttempt, id.RunAttempt},
		{KeyCheckRunID, id.CheckRunID},
		{KeyRepository, id.Repository},
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

// Runner resource attribute keys. Unlike the identity keys above, these
// describe the machine a job runs on rather than the job itself, so
// gauger-server has nowhere to store them per job yet: they are resource
// attributes on metric batches only and never appear in the lifecycle body.
const (
	KeyCPUModel           = "host.cpu.model.name"
	KeyOSImage            = "os.image"
	KeyRunnerImageVersion = "github.runner.image_version"
	KeyRunnerEnvironment  = "github.runner.environment"
)

// RunnerAttributes names the machine a job runs on.
type RunnerAttributes struct {
	CPUModel     string
	OSImage      string
	ImageVersion string
	Environment  string
}

// RunnerAttributesFromEnv reads the runner's image and environment from the
// environment. cpuModel comes from /proc/cpuinfo, which is procfs's job to
// read, not this package's.
func RunnerAttributesFromEnv(getenv func(string) string, cpuModel string) RunnerAttributes {
	return RunnerAttributes{
		CPUModel:     cpuModel,
		OSImage:      getenv("ImageOS"),
		ImageVersion: getenv("ImageVersion"),
		Environment:  getenv("RUNNER_ENVIRONMENT"),
	}
}

// Attributes returns the runner attributes as OTel resource attributes.
// Empty values are left out, same as Identity.Attributes.
func (r RunnerAttributes) Attributes() []Attribute {
	all := []Attribute{
		{KeyCPUModel, r.CPUModel},
		{KeyOSImage, r.OSImage},
		{KeyRunnerImageVersion, r.ImageVersion},
		{KeyRunnerEnvironment, r.Environment},
	}
	attrs := all[:0]
	for _, a := range all {
		if a.Value != "" {
			attrs = append(attrs, a)
		}
	}
	return attrs
}
