package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/shlex"
	"github.com/google/uuid"
	"github.com/spf13/pflag"

	"github.com/mitchellh/go-homedir"
	log "github.com/sirupsen/logrus"

	selinux "github.com/opencontainers/selinux/go-selinux"

	"github.com/ankit-arora/act/pkg/common"
	"github.com/ankit-arora/act/pkg/container"
	"github.com/ankit-arora/act/pkg/model"
)

// RunContext contains info about current job
type RunContext struct {
	GithubContextBase *string
	Name              string
	Config            *Config
	Matrix            map[string]interface{}
	Run               *model.Run
	EventJSON         string
	Env               map[string]string
	ExtraPath         []string
	CurrentStep       string
	StepResults       map[string]*model.StepResult
	ExprEval          ExpressionEvaluator
	JobContainer      container.Container
	OutputMappings    map[MappableOutput]MappableOutput
	JobName           string
	actPath           string
	Local             bool
	ActionPath        string
	ActionRef         string
	ActionRepository  string
	Composite         *model.Action
	Inputs            map[string]interface{}
	Parent            *RunContext
	ContextData       map[string]interface{}
}

func (rc *RunContext) Clone() *RunContext {
	clone := *rc
	clone.CurrentStep = ""
	clone.Composite = nil
	clone.Inputs = nil
	clone.StepResults = make(map[string]*model.StepResult)
	clone.Parent = rc
	if rc.ContextData != nil {
		clone.ContextData = map[string]interface{}{
			"github": rc.ContextData["github"],
		}
	}
	return &clone
}

func (rc *RunContext) SetActPath(actPath string) {
	rc.actPath = actPath
}

func (rc *RunContext) GetActPath() string {
	if len(rc.actPath) > 0 {
		return rc.actPath
	}
	return "/var/run/act"
}

type MappableOutput struct {
	StepID     string
	OutputName string
}

func (rc *RunContext) String() string {
	return fmt.Sprintf("%s/%s", rc.Run.Workflow.Name, rc.Name)
}

// GetEnv returns the env for the context
func (rc *RunContext) GetEnv() map[string]string {
	if rc.Env == nil {
		rc.Env = mergeMaps(rc.Config.Env, rc.Run.Workflow.Env, rc.Run.Job().Environment())
	}
	rc.Env["ACT"] = "true"
	return rc.Env
}

func (rc *RunContext) jobContainerName() string {
	return createContainerName("act", rc.String())
}

// Returns the binds and mounts for the container, resolving paths as appopriate
func (rc *RunContext) GetBindsAndMounts() ([]string, map[string]string) {
	name := rc.jobContainerName()

	if rc.Config.ContainerDaemonSocket == "" {
		rc.Config.ContainerDaemonSocket = "/var/run/docker.sock"
	}

	binds := []string{
		fmt.Sprintf("%s:%s", rc.Config.ContainerDaemonSocket, "/var/run/docker.sock"),
	}

	mounts := map[string]string{
		"act-toolcache": "/toolcache",
		name + "-env":   rc.GetActPath(),
	}

	if rc.Config.BindWorkdir {
		bindModifiers := ""
		if runtime.GOOS == "darwin" {
			bindModifiers = ":delegated"
		}
		if selinux.GetEnabled() {
			bindModifiers = ":z"
		}
		binds = append(binds, fmt.Sprintf("%s:%s%s", rc.Config.Workdir, rc.ContainerWorkdir(), bindModifiers))
	} else {
		mounts[name] = rc.ContainerWorkdir()
	}

	return binds, mounts
}

func (rc *RunContext) startJobContainer() common.Executor {
	image := rc.platformImage()
	if image == "-self-hosted" {
		return func(ctx context.Context) error {
			rawLogger := common.Logger(ctx).WithField("raw_output", true)
			logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
				if rc.Config.LogOutput {
					rawLogger.Infof("%s", s)
				} else {
					rawLogger.Debugf("%s", s)
				}
				return true
			})
			cacheDir := rc.ActionCacheDir()
			miscpath := filepath.Join(cacheDir, uuid.New().String())
			actPath := filepath.Join(miscpath, "act")
			if err := os.MkdirAll(actPath, 0777); err != nil {
				return err
			}
			rc.SetActPath(actPath)
			path := filepath.Join(miscpath, "hostexecutor")
			if err := os.MkdirAll(path, 0777); err != nil {
				return err
			}
			runnerTmp := filepath.Join(miscpath, "tmp")
			if err := os.MkdirAll(runnerTmp, 0777); err != nil {
				return err
			}
			rc.JobContainer = &container.HostExecutor{Path: path, CleanUp: func() {
				os.RemoveAll(miscpath)
			}, StdOut: logWriter}
			var copyWorkspace bool
			var copyToPath string
			if !rc.Config.BindWorkdir {
				copyToPath, copyWorkspace = rc.localCheckoutPath()
				copyToPath = filepath.Join(path, copyToPath)
			}
			// Tell act to not change the filepath on windows
			rc.Local = true
			rc.Env["RUNNER_TOOL_CACHE"] = filepath.Join(cacheDir, "tool_cache")
			rc.Env["RUNNER_OS"] = runtime.GOOS
			rc.Env["RUNNER_ARCH"] = runtime.GOARCH
			rc.Env["RUNNER_TEMP"] = runnerTmp
			for _, env := range os.Environ() {
				i := strings.Index(env, "=")
				if i > 0 {
					rc.Env[env[0:i]] = env[i+1:]
				}
			}

			return common.NewPipelineExecutor(
				rc.JobContainer.CopyDir(copyToPath, rc.Config.Workdir+string(filepath.Separator)+".", rc.Config.UseGitIgnore).IfBool(copyWorkspace),
				rc.JobContainer.Copy(rc.GetActPath()+"/", &container.FileEntry{
					Name: "workflow/event.json",
					Mode: 0644,
					Body: rc.EventJSON,
				}, &container.FileEntry{
					Name: "workflow/envs.txt",
					Mode: 0666,
					Body: "",
				}, &container.FileEntry{
					Name: "workflow/paths.txt",
					Mode: 0666,
					Body: "",
				}),
			)(ctx)
		}
	}
	hostname := rc.hostname()

	return func(ctx context.Context) error {
		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		username, password, err := rc.handleCredentials()
		if err != nil {
			return fmt.Errorf("failed to handle credentials: %s", err)
		}

		common.Logger(ctx).Infof("\U0001f680  Start image=%s", image)
		name := rc.jobContainerName()

		envList := make([]string, 0)

		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TOOL_CACHE", "/opt/hostedtoolcache"))
		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_OS", "Linux"))
		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TEMP", "/tmp"))

		binds, mounts := rc.GetBindsAndMounts()

		rc.JobContainer = container.NewContainer(&container.NewContainerInput{
			Cmd:         nil,
			Entrypoint:  []string{"/usr/bin/tail", "-f", "/dev/null"},
			WorkingDir:  rc.ContainerWorkdir(),
			Image:       image,
			Username:    username,
			Password:    password,
			Name:        name,
			Env:         envList,
			Mounts:      mounts,
			NetworkMode: "host",
			Binds:       binds,
			Stdout:      logWriter,
			Stderr:      logWriter,
			Privileged:  rc.Config.Privileged,
			UsernsMode:  rc.Config.UsernsMode,
			Platform:    rc.Config.ContainerArchitecture,
			Hostname:    hostname,
		})

		if rc.JobContainer == nil {
			return errors.New("failed to create Container")
		}

		var copyWorkspace bool
		var copyToPath string
		if !rc.Config.BindWorkdir {
			copyToPath, copyWorkspace = rc.localCheckoutPath()
			copyToPath = filepath.Join(rc.ContainerWorkdir(), copyToPath)
		}

		return common.NewPipelineExecutor(
			rc.JobContainer.Pull(rc.Config.ForcePull),
			rc.stopJobContainer(),
			rc.JobContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			rc.JobContainer.Start(false),
			rc.JobContainer.UpdateFromImageEnv(&rc.Env),
			rc.JobContainer.UpdateFromEnv("/etc/environment", &rc.Env),
			rc.JobContainer.Exec([]string{"mkdir", "-m", "0777", "-p", rc.GetActPath()}, "", rc.Env, "root", ""),
			rc.JobContainer.CopyDir(copyToPath, rc.Config.Workdir+string(filepath.Separator)+".", rc.Config.UseGitIgnore).IfBool(copyWorkspace),
			rc.JobContainer.Copy(rc.GetActPath()+"/", &container.FileEntry{
				Name: "workflow/event.json",
				Mode: 0644,
				Body: rc.EventJSON,
			}, &container.FileEntry{
				Name: "workflow/envs.txt",
				Mode: 0666,
				Body: "",
			}, &container.FileEntry{
				Name: "workflow/paths.txt",
				Mode: 0666,
				Body: "",
			}),
		)(ctx)
	}
}
func (rc *RunContext) execJobContainer(cmd []string, cmdline string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		return rc.JobContainer.Exec(cmd, cmdline, env, user, workdir)(ctx)
	}
}

// stopJobContainer removes the job container (if it exists) and its volume (if it exists) if !rc.Config.ReuseContainers
func (rc *RunContext) stopJobContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.JobContainer != nil && !rc.Config.ReuseContainers {
			return rc.JobContainer.Remove().
				Then(container.NewDockerVolumeRemoveExecutor(rc.jobContainerName(), false).Finally(container.NewDockerVolumeRemoveExecutor(rc.jobContainerName()+"-env", false)).If(func(ctx context.Context) bool { return !rc.Local }))(ctx)
		}
		return nil
	}
}

// Prepare the mounts and binds for the worker

// ActionCacheDir is for rc
func (rc *RunContext) ActionCacheDir() string {
	var xdgCache string
	var ok bool
	if xdgCache, ok = os.LookupEnv("XDG_CACHE_HOME"); !ok || xdgCache == "" {
		if home, err := homedir.Dir(); err == nil {
			xdgCache = filepath.Join(home, ".cache")
		} else if xdgCache, err = filepath.Abs("."); err != nil {
			log.Fatal(err)
		}
	}
	return filepath.Join(xdgCache, "act")
}

// Interpolate outputs after a job is done
func (rc *RunContext) interpolateOutputs() common.Executor {
	return func(ctx context.Context) error {
		ee := rc.NewExpressionEvaluator()
		for k, v := range rc.Run.Job().Outputs {
			interpolated := ee.Interpolate(v)
			if v != interpolated {
				rc.Run.Job().Outputs[k] = interpolated
			}
		}
		return nil
	}
}

func (rc *RunContext) startContainer() common.Executor {
	return rc.startJobContainer()
}

func (rc *RunContext) stopContainer() common.Executor {
	return rc.stopJobContainer()
}

func (rc *RunContext) closeContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.JobContainer != nil {
			return rc.JobContainer.Close()(ctx)
		}
		return nil
	}
}

func (rc *RunContext) matrix() map[string]interface{} {
	return rc.Matrix
}

func (rc *RunContext) result(result string) {
	rc.Run.Job().Result = result
}

func (rc *RunContext) steps() []*model.Step {
	return rc.Run.Job().Steps
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) Executor() common.Executor {
	return newJobExecutor(rc).Finally(func(ctx context.Context) error {
		if rc.JobContainer != nil {
			ctx := context.Background()
			if rc.Config.AutoRemove {
				log.Infof("Cleaning up container for job %s", rc.JobName)
				if err := rc.stopJobContainer()(ctx); err != nil {
					log.Errorf("Error while cleaning container: %v", err)
				}
			}
			return rc.JobContainer.Close()(ctx)
		}
		return nil
	}).If(rc.isEnabled)
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) CompositeExecutor() common.Executor {
	steps := make([]common.Executor, 0)

	for i, step := range rc.Composite.Runs.Steps {
		if step.ID == "" {
			step.ID = fmt.Sprintf("%d", i)
		}
		stepcopy := step
		stepExec := rc.newStepExecutor(&stepcopy)
		steps = append(steps, func(ctx context.Context) error {
			err := stepExec(ctx)
			if err != nil {
				common.Logger(ctx).Errorf("%v", err)
				common.SetJobError(ctx, err)
			} else if ctx.Err() != nil {
				common.Logger(ctx).Errorf("%v", ctx.Err())
				common.SetJobError(ctx, ctx.Err())
			}
			return nil
		})
	}

	steps = append(steps, common.JobError)
	return func(ctx context.Context) error {
		return common.NewPipelineExecutor(steps...)(common.WithJobErrorContainer(ctx))
	}
}

func (rc *RunContext) newStepExecutor(step *model.Step) common.Executor {
	sc := &StepContext{
		RunContext: rc,
		Step:       step,
	}
	return func(ctx context.Context) error {
		rc.CurrentStep = sc.Step.ID
		rc.StepResults[rc.CurrentStep] = &model.StepResult{
			Outcome:    model.StepStatusSuccess,
			Conclusion: model.StepStatusSuccess,
			Outputs:    make(map[string]string),
		}

		runStep, err := sc.isEnabled(ctx)
		if err != nil {
			rc.StepResults[rc.CurrentStep].Conclusion = model.StepStatusFailure
			rc.StepResults[rc.CurrentStep].Outcome = model.StepStatusFailure
			return err
		}

		if !runStep {
			log.Debugf("Skipping step '%s' due to '%s'", sc.Step.String(), sc.Step.If.Value)
			rc.StepResults[rc.CurrentStep].Conclusion = model.StepStatusSkipped
			rc.StepResults[rc.CurrentStep].Outcome = model.StepStatusSkipped
			return nil
		}

		exprEval, err := sc.setupEnv(ctx)
		if err != nil {
			return err
		}
		rc.ExprEval = exprEval

		common.Logger(ctx).Infof("\u2B50  Run %s", sc.Step)

		// Prepare and clean Runner File Commands
		actPath := rc.GetActPath()
		outputFileCommand := path.Join("workflow", "outputcmd.txt")
		stateFileCommand := path.Join("workflow", "statecmd.txt")
		sc.Env["GITHUB_OUTPUT"] = path.Join(actPath, outputFileCommand)
		sc.Env["GITHUB_STATE"] = path.Join(actPath, stateFileCommand)
		_ = rc.JobContainer.Copy(actPath, &container.FileEntry{
			Name: outputFileCommand,
			Mode: 0666,
		}, &container.FileEntry{
			Name: stateFileCommand,
			Mode: 0666,
		})(ctx)

		err = sc.Executor(ctx)(ctx)
		if err == nil {
			common.Logger(ctx).Infof("  \u2705  Success - %s", sc.Step)
		} else {
			common.Logger(ctx).Errorf("  \u274C  Failure - %s", sc.Step)

			rc.StepResults[rc.CurrentStep].Outcome = model.StepStatusFailure
			if sc.Step.ContinueOnError {
				common.Logger(ctx).Infof("Failed but continue next step")
				err = nil
				rc.StepResults[rc.CurrentStep].Conclusion = model.StepStatusSuccess
			} else {
				rc.StepResults[rc.CurrentStep].Conclusion = model.StepStatusFailure
			}
		}
		// Process Runner File Commands
		orgerr := err
		output := map[string]string{}
		err = rc.JobContainer.UpdateFromEnv(path.Join(actPath, outputFileCommand), &output)(ctx)
		if err != nil {
			return err
		}
		for k, v := range output {
			rc.setOutput(ctx, map[string]string{"name": k}, v)
		}
		if orgerr != nil {
			return orgerr
		}
		return err
	}
}

func (rc *RunContext) platformImage() string {
	job := rc.Run.Job()

	c := job.Container()
	if c != nil {
		return rc.ExprEval.Interpolate(c.Image)
	}

	if job.RunsOn() == nil {
		log.Errorf("'runs-on' key not defined in %s", rc.String())
	}

	for _, runnerLabel := range job.RunsOn() {
		platformName := rc.ExprEval.Interpolate(runnerLabel)
		image := rc.Config.Platforms[strings.ToLower(platformName)]
		if image != "" {
			return image
		}
	}

	return ""
}

func (rc *RunContext) hostname() string {
	job := rc.Run.Job()
	c := job.Container()
	if c == nil {
		return ""
	}

	optionsFlags := pflag.NewFlagSet("container_options", pflag.ContinueOnError)
	hostname := optionsFlags.StringP("hostname", "h", "", "")
	optionsArgs, err := shlex.Split(c.Options)
	if err != nil {
		log.Warnf("Cannot parse container options: %s", c.Options)
		return ""
	}
	err = optionsFlags.Parse(optionsArgs)
	if err != nil {
		log.Warnf("Cannot parse container options: %s", c.Options)
		return ""
	}
	return *hostname
}

func (rc *RunContext) isEnabled(ctx context.Context) bool {
	job := rc.Run.Job()
	l := common.Logger(ctx)
	runJob, err := EvalBool(rc.ExprEval, job.If.Value)
	if err != nil {
		common.Logger(ctx).Errorf("  \u274C  Error in if: expression - %s", job.Name)
		return false
	}
	if !runJob {
		l.Debugf("Skipping job '%s' due to '%s'", job.Name, job.If.Value)
		return false
	}

	img := rc.platformImage()
	if img == "" {
		if job.RunsOn() == nil {
			log.Errorf("'runs-on' key not defined in %s", rc.String())
		}

		for _, runnerLabel := range job.RunsOn() {
			platformName := rc.ExprEval.Interpolate(runnerLabel)
			l.Infof("\U0001F6A7  Skipping unsupported platform -- Try running with `-P %+v=...`", platformName)
		}
		return false
	}
	return true
}

func mergeMaps(maps ...map[string]string) map[string]string {
	rtnMap := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			rtnMap[k] = v
		}
	}
	return rtnMap
}

func createContainerName(parts ...string) string {
	name := make([]string, 0)
	pattern := regexp.MustCompile("[^a-zA-Z0-9]")
	partLen := (30 / len(parts)) - 1
	for i, part := range parts {
		if i == len(parts)-1 {
			name = append(name, pattern.ReplaceAllString(part, "-"))
		} else {
			// If any part has a '-<number>' on the end it is likely part of a matrix job.
			// Let's preserve the number to prevent clashes in container names.
			re := regexp.MustCompile("-[0-9]+$")
			num := re.FindStringSubmatch(part)
			if len(num) > 0 {
				name = append(name, trimToLen(pattern.ReplaceAllString(part, "-"), partLen-len(num[0])))
				name = append(name, num[0])
			} else {
				name = append(name, trimToLen(pattern.ReplaceAllString(part, "-"), partLen))
			}
		}
	}
	return strings.ReplaceAll(strings.Trim(strings.Join(name, "-"), "-"), "--", "-")
}

func trimToLen(s string, l int) string {
	if l < 0 {
		l = 0
	}
	if len(s) > l {
		return s[:l]
	}
	return s
}

func (rc *RunContext) getJobContext() *model.JobContext {
	jobStatus := "success"
	for _, stepStatus := range rc.StepResults {
		if stepStatus.Conclusion == model.StepStatusFailure {
			jobStatus = "failure"
			break
		}
	}
	return &model.JobContext{
		Status: jobStatus,
	}
}

func (rc *RunContext) getStepsContext() map[string]*model.StepResult {
	return rc.StepResults
}

func (rc *RunContext) getGithubContext() *model.GithubContext {
	ghc := &model.GithubContext{
		Event:            make(map[string]interface{}),
		EventPath:        rc.GetActPath() + "/workflow/event.json",
		Workflow:         rc.Run.Workflow.Name,
		RunID:            rc.Config.Env["GITHUB_RUN_ID"],
		RunNumber:        rc.Config.Env["GITHUB_RUN_NUMBER"],
		RunAttempt:       rc.Config.Env["GITHUB_RUN_ATTEMPT"],
		Actor:            rc.Config.Actor,
		EventName:        rc.Config.EventName,
		Workspace:        rc.ContainerWorkdir(),
		Action:           rc.CurrentStep,
		Token:            rc.Config.Secrets["GITHUB_TOKEN"],
		ActionPath:       rc.ActionPath,
		ActionRef:        rc.ActionRef,
		ActionRepository: rc.ActionRepository,
		RepositoryOwner:  rc.Config.Env["GITHUB_REPOSITORY_OWNER"],
		RetentionDays:    rc.Config.Env["GITHUB_RETENTION_DAYS"],
		RunnerPerflog:    rc.Config.Env["RUNNER_PERFLOG"],
		RunnerTrackingID: rc.Config.Env["RUNNER_TRACKING_ID"],
	}
	if rc.GithubContextBase != nil {
		err := json.Unmarshal([]byte(*rc.GithubContextBase), ghc)
		if err == nil {
			return ghc
		}
	}
	return applyDefaults(ghc, rc)
}

func applyDefaults(ghc *model.GithubContext, rc *RunContext) *model.GithubContext {
	if ghc.RunID == "" {
		ghc.RunID = "1"
	}

	if ghc.RunNumber == "" {
		ghc.RunNumber = "1"
	}

	if ghc.RunAttempt == "" {
		ghc.RunAttempt = "1"
	}

	if ghc.RetentionDays == "" {
		ghc.RetentionDays = "0"
	}

	if ghc.RunnerPerflog == "" {
		ghc.RunnerPerflog = "/dev/null"
	}

	// Backwards compatibility for configs that require
	// a default rather than being run as a cmd
	if ghc.Actor == "" {
		ghc.Actor = "nektos/act"
	}

	repoPath := rc.Config.Workdir
	repo, err := common.FindGithubRepo(repoPath, rc.Config.GitHubInstance)
	if err != nil {
		log.Warningf("unable to get git repo: %v", err)
	} else {
		ghc.Repository = repo
		if ghc.RepositoryOwner == "" {
			ghc.RepositoryOwner = strings.Split(repo, "/")[0]
		}
	}

	if rc.EventJSON != "" {
		err = json.Unmarshal([]byte(rc.EventJSON), &ghc.Event)
		if err != nil {
			log.Errorf("Unable to Unmarshal event '%s': %v", rc.EventJSON, err)
		}
	}

	if ghc.EventName == "pull_request" {
		ghc.BaseRef = asString(nestedMapLookup(ghc.Event, "pull_request", "base", "ref"))
		ghc.HeadRef = asString(nestedMapLookup(ghc.Event, "pull_request", "head", "ref"))
	}

	ghc.SetRefAndSha(rc.Config.DefaultBranch, repoPath)

	return ghc
}

func isLocalCheckout(ghc *model.GithubContext, step *model.Step) bool {
	if step.Type() == model.StepTypeInvalid {
		// This will be errored out by the executor later, we need this here to avoid a null panic though
		return false
	}
	if step.Type() != model.StepTypeUsesActionRemote {
		return false
	}
	remoteAction := newRemoteAction(step.Uses)
	if remoteAction == nil {
		// IsCheckout() will nil panic if we dont bail out early
		return false
	}
	if !remoteAction.IsCheckout() {
		return false
	}

	if repository, ok := step.With["repository"]; ok && repository != ghc.Repository {
		return false
	}
	if repository, ok := step.With["ref"]; ok && repository != ghc.Ref {
		return false
	}
	return true
}

func asString(v interface{}) string {
	if v == nil {
		return ""
	} else if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func nestedMapLookup(m map[string]interface{}, ks ...string) (rval interface{}) {
	var ok bool

	if len(ks) == 0 { // degenerate input
		return nil
	}
	if rval, ok = m[ks[0]]; !ok {
		return nil
	} else if len(ks) == 1 { // we've reached the final key
		return rval
	} else if m, ok = rval.(map[string]interface{}); !ok {
		return nil
	} else { // 1+ more keys
		return nestedMapLookup(m, ks[1:]...)
	}
}

func (rc *RunContext) withGithubEnv(env map[string]string) map[string]string {
	github := rc.getGithubContext()
	env["CI"] = "true"
	env["GITHUB_ENV"] = rc.GetActPath() + "/workflow/envs.txt"
	env["GITHUB_PATH"] = rc.GetActPath() + "/workflow/paths.txt"
	env["GITHUB_WORKFLOW"] = github.Workflow
	env["GITHUB_RUN_ID"] = github.RunID
	env["GITHUB_RUN_NUMBER"] = github.RunNumber
	env["GITHUB_RUN_ATTEMPT"] = github.RunAttempt
	env["GITHUB_ACTION"] = github.Action
	env["GITHUB_ACTION_PATH"] = github.ActionPath
	env["GITHUB_ACTION_REPOSITORY"] = github.ActionRepository
	env["GITHUB_ACTION_REF"] = github.ActionRef
	env["GITHUB_ACTIONS"] = "true"
	env["GITHUB_ACTOR"] = github.Actor
	env["GITHUB_REPOSITORY"] = github.Repository
	env["GITHUB_EVENT_NAME"] = github.EventName
	env["GITHUB_EVENT_PATH"] = github.EventPath
	env["GITHUB_WORKSPACE"] = github.Workspace
	env["GITHUB_SHA"] = github.Sha
	env["GITHUB_REF"] = github.Ref
	env["GITHUB_TOKEN"] = github.Token
	env["GITHUB_SERVER_URL"] = "https://github.com"
	env["GITHUB_API_URL"] = "https://api.github.com"
	env["GITHUB_GRAPHQL_URL"] = "https://api.github.com/graphql"
	env["GITHUB_ACTION_REF"] = github.ActionRef
	env["GITHUB_ACTION_REPOSITORY"] = github.ActionRepository
	env["GITHUB_BASE_REF"] = github.BaseRef
	env["GITHUB_HEAD_REF"] = github.HeadRef
	env["GITHUB_JOB"] = rc.JobName
	env["GITHUB_REPOSITORY_OWNER"] = github.RepositoryOwner
	env["GITHUB_RETENTION_DAYS"] = github.RetentionDays
	env["RUNNER_PERFLOG"] = github.RunnerPerflog
	env["RUNNER_TRACKING_ID"] = github.RunnerTrackingID
	if rc.Config.GitHubInstance != "github.com" {
		env["GITHUB_SERVER_URL"] = fmt.Sprintf("https://%s", rc.Config.GitHubInstance)
		env["GITHUB_API_URL"] = fmt.Sprintf("https://%s/api/v3", rc.Config.GitHubInstance)
		env["GITHUB_GRAPHQL_URL"] = fmt.Sprintf("https://%s/api/graphql", rc.Config.GitHubInstance)
	}
	if len(rc.Config.GitHubServerUrl) > 0 {
		env["GITHUB_SERVER_URL"] = rc.Config.GitHubServerUrl
	}
	if len(rc.Config.GitHubServerUrl) > 0 {
		env["GITHUB_API_URL"] = rc.Config.GitHubApiServerUrl
	}
	if len(rc.Config.GitHubServerUrl) > 0 {
		env["GITHUB_GRAPHQL_URL"] = rc.Config.GitHubGraphQlApiServerUrl
	}

	if rc.Config.ArtifactServerPath != "" {
		setActionRuntimeVars(rc, env)
	}

	job := rc.Run.Job()
	if job.RunsOn() != nil {
		for _, runnerLabel := range job.RunsOn() {
			platformName := rc.ExprEval.Interpolate(runnerLabel)
			if platformName != "" {
				if platformName == "ubuntu-latest" {
					// hardcode current ubuntu-latest since we have no way to check that 'on the fly'
					env["ImageOS"] = "ubuntu20"
				} else {
					platformName = strings.SplitN(strings.Replace(platformName, `-`, ``, 1), `.`, 2)[0]
					env["ImageOS"] = platformName
				}
			}
		}
	}

	return env
}

func setActionRuntimeVars(rc *RunContext, env map[string]string) {
	actionsRuntimeURL := os.Getenv("ACTIONS_RUNTIME_URL")
	if actionsRuntimeURL == "" {
		actionsRuntimeURL = fmt.Sprintf("http://%s:%s/", common.GetOutboundIP().String(), rc.Config.ArtifactServerPort)
	}
	env["ACTIONS_RUNTIME_URL"] = actionsRuntimeURL

	actionsRuntimeToken := os.Getenv("ACTIONS_RUNTIME_TOKEN")
	if actionsRuntimeToken == "" {
		actionsRuntimeToken = "token"
	}
	env["ACTIONS_RUNTIME_TOKEN"] = actionsRuntimeToken
}

func (rc *RunContext) localCheckoutPath() (string, bool) {
	if rc.Config.ForceRemoteCheckout {
		return "", false
	}
	ghContext := rc.getGithubContext()
	for _, step := range rc.Run.Job().Steps {
		if isLocalCheckout(ghContext, step) {
			return step.With["path"], true
		}
	}
	return "", false
}

func (rc *RunContext) handleCredentials() (username, password string, err error) {
	// TODO: remove below 2 lines when we can release act with breaking changes
	username = rc.Config.Secrets["DOCKER_USERNAME"]
	password = rc.Config.Secrets["DOCKER_PASSWORD"]

	container := rc.Run.Job().Container()
	if container == nil || container.Credentials == nil {
		return
	}

	if container.Credentials != nil && len(container.Credentials) != 2 {
		err = fmt.Errorf("invalid property count for key 'credentials:'")
		return
	}

	ee := rc.NewExpressionEvaluator()
	if username = ee.Interpolate(container.Credentials["username"]); username == "" {
		err = fmt.Errorf("failed to interpolate container.credentials.username")
		return
	}
	if password = ee.Interpolate(container.Credentials["password"]); password == "" {
		err = fmt.Errorf("failed to interpolate container.credentials.password")
		return
	}

	if container.Credentials["username"] == "" || container.Credentials["password"] == "" {
		err = fmt.Errorf("container.credentials cannot be empty")
		return
	}

	return username, password, err
}
