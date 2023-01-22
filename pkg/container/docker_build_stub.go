//go:build !linux && !darwin && !windows && !openbsd
// +build !linux,!darwin,!windows,!openbsd

package container

import (
	"context"
	"errors"

	// github.com/docker/docker/builder/dockerignore is deprecated

	"github.com/ankit-arora/act/pkg/common"
)

// NewDockerBuildExecutorInput the input for the NewDockerBuildExecutor function
type NewDockerBuildExecutorInput struct {
	ContextDir string
	Container  Container
	ImageTag   string
	Platform   string
}

// NewDockerBuildExecutor function to create a run executor for the container
func NewDockerBuildExecutor(input NewDockerBuildExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}
