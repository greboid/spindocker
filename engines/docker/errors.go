package docker

import "errors"

var (
	ErrOOMKilled = errors.New("oom killed")
)
