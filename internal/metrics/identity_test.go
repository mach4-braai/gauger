package metrics

import (
	"reflect"
	"testing"
)

func TestRunnerAttributesFromEnv(t *testing.T) {
	env := map[string]string{
		"ImageOS":            "ubuntu24",
		"ImageVersion":       "20260101.1",
		"RUNNER_ENVIRONMENT": "github-hosted",
	}
	got := RunnerAttributesFromEnv(func(k string) string { return env[k] }, "Intel(R) Xeon(R) Platinum 8272CL CPU @ 2.60GHz")
	want := RunnerAttributes{
		CPUModel:     "Intel(R) Xeon(R) Platinum 8272CL CPU @ 2.60GHz",
		OSImage:      "ubuntu24",
		ImageVersion: "20260101.1",
		Environment:  "github-hosted",
	}
	if got != want {
		t.Fatalf("RunnerAttributesFromEnv = %+v, want %+v", got, want)
	}
}

func TestRunnerAttributesAttributesLeavesOutEmptyValues(t *testing.T) {
	got := RunnerAttributes{
		CPUModel:     "0x41 0xd0c",
		OSImage:      "",
		ImageVersion: "20260101.1",
		Environment:  "",
	}.Attributes()
	want := []Attribute{
		{KeyCPUModel, "0x41 0xd0c"},
		{KeyRunnerImageVersion, "20260101.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Attributes = %+v, want %+v", got, want)
	}
}

func TestRunnerAttributesAttributesEmptyWhenAllEmpty(t *testing.T) {
	got := RunnerAttributes{}.Attributes()
	if len(got) != 0 {
		t.Fatalf("Attributes = %+v, want empty", got)
	}
}
