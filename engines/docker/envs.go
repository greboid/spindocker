package docker

import "fmt"

type EnvVars []string

func ConstructEnvs(envs map[string]string) EnvVars {
	var dockerEnvs EnvVars
	for k, v := range envs {
		dockerEnvs = append(dockerEnvs, fmt.Sprintf("%s=%s", k, v))
	}
	return dockerEnvs
}

func (ev EnvVars) Slice() []string { return ev }

func (ev *EnvVars) AddEnv(key, value string) {
	*ev = append(*ev, fmt.Sprintf("%s=%s", key, value))
}
