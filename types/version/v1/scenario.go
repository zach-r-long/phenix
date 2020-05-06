package v1

type ScenarioSpec struct {
	Apps *Apps `json:"apps" yaml:"apps"`
}

type Apps struct {
	Experiment []ExperimentApp `json:"experiment" yaml:"experiment"`
	Host       []HostApp       `json:"host" yaml:"host"`
}

type ExperimentApp struct {
	Name     string                 `json:"name" yaml:"name"`
	Metadata map[string]interface{} `json:"metadata" yaml:"metadata"`
}

type HostApp struct {
	Name  string `json:"name" yaml:"name"`
	Hosts []Host `json:"hosts" yaml:"hosts"`
}

type Host struct {
	Hostname string                 `json:"hostname" yaml:"hostname"`
	Metadata map[string]interface{} `json:"metadata" yaml:"metadata"`
}
