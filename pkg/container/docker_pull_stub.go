//go:build !linux && !darwin && !windows && !openbsd
// +build !linux,!darwin,!windows,!openbsd

package container

import (
	"context"
	"errors"

	"github.com/ankit-arora/act/pkg/common"
)

// NewDockerPullExecutorInput the input for the NewDockerPullExecutor function
type NewDockerPullExecutorInput struct {
	Image     string
	ForcePull bool
	Platform  string
	Username  string
	Password  string
}

// NewDockerPullExecutor function to create a run executor for the container
func NewDockerPullExecutor(input NewDockerPullExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		return errors.New("Unsupported Operation")
	}
}
