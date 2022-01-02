package container

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nektos/act/pkg/common"
	"golang.org/x/term"
)

func getEnvListFromMap(env map[string]string) []string {
	envList := make([]string, 0)
	for k, v := range env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}
	return envList
}

// NewContainerInput the input for the New function
type NewContainerInput struct {
	Image       string
	Username    string
	Password    string
	Entrypoint  []string
	Cmd         []string
	WorkingDir  string
	Env         []string
	Binds       []string
	Mounts      map[string]string
	Name        string
	Stdout      io.Writer
	Stderr      io.Writer
	NetworkMode string
	Privileged  bool
	UsernsMode  string
	Platform    string
	Hostname    string
}

// FileEntry is a file to copy to a container
type FileEntry struct {
	Name string
	Mode int64
	Body string
}

// Container for managing docker run containers
type Container interface {
	Create(capAdd []string, capDrop []string) common.Executor
	Copy(destPath string, files ...*FileEntry) common.Executor
	CopyDir(destPath string, srcPath string, useGitIgnore bool) common.Executor
	GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error)
	Pull(forcePull bool) common.Executor
	Start(attach bool) common.Executor
	Exec(command []string, cmdline string, env map[string]string, user, workdir string) common.Executor
	UpdateFromEnv(srcPath string, env *map[string]string) common.Executor
	UpdateFromImageEnv(env *map[string]string) common.Executor
	UpdateFromPath(env *map[string]string) common.Executor
	Remove() common.Executor
	Close() common.Executor
}

var containerAllocateTerminal bool

func init() {
	containerAllocateTerminal = term.IsTerminal(int(os.Stdout.Fd()))
}

func SetContainerAllocateTerminal(val bool) {
	containerAllocateTerminal = val
}
