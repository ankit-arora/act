//go:build !linux && !darwin && !windows && !openbsd
// +build !linux,!darwin,!windows,!openbsd

package container

import (
	"context"

	"github.com/ankit-arora/act/pkg/common"
)

func NewDockerVolumeRemoveExecutor(volume string, force bool) common.Executor {
	return func(ctx context.Context) error {
		return nil
	}
}
