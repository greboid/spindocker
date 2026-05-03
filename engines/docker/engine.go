package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"

	"tangled.org/core/api/tangled"
	"tangled.org/core/log"
	"tangled.org/core/spindle/config"
	"tangled.org/core/spindle/engine"
	"tangled.org/core/spindle/models"
	"tangled.org/core/spindle/secrets"
)

const (
	workspaceDir     = "/tangled/workspace"
	homeDir          = "/tangled/home"
	defaultImage     = "debian:bookworm-slim"
	defaultTimeout   = "5m"
	envPrefix        = "SPINDLE_DOCKER_PIPELINES_"
	envDefaultImage  = envPrefix + "DEFAULT_IMAGE"
	envWorkflowTimeout = envPrefix + "WORKFLOW_TIMEOUT"
)

type cleanupFunc func(context.Context) error

type Engine struct {
	docker          client.APIClient
	l               *slog.Logger
	dev             bool
	defaultImage    string
	workflowTimeout string

	cleanupMu sync.Mutex
	cleanup   map[string][]cleanupFunc
}

type Step struct {
	name        string
	kind        models.StepKind
	command     string
	environment map[string]string
}

func (s Step) Name() string         { return s.name }
func (s Step) Command() string      { return s.command }
func (s Step) Kind() models.StepKind { return s.kind }

type addlFields struct {
	image     string
	container string
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func New(ctx context.Context, cfg *config.Config) (*Engine, error) {
	dcli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	l := log.FromContext(ctx).With("component", "spindle-docker")

	e := &Engine{
		docker:          dcli,
		l:               l,
		dev:             cfg.Server.Dev,
		defaultImage:    getEnv(envDefaultImage, defaultImage),
		workflowTimeout: getEnv(envWorkflowTimeout, defaultTimeout),
		cleanup:         make(map[string][]cleanupFunc),
	}

	return e, nil
}

type workflowManifest struct {
	Steps []struct {
		Command     string            `yaml:"command"`
		Name        string            `yaml:"name"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"steps"`
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment"`
}

func (e *Engine) InitWorkflow(twf tangled.Pipeline_Workflow, tpl tangled.Pipeline) (*models.Workflow, error) {
	dwf := &workflowManifest{}
	if err := yaml.Unmarshal([]byte(twf.Raw), &dwf); err != nil {
		return nil, err
	}

	swf := &models.Workflow{}
	addl := addlFields{}

	baseImage := dwf.Image
	if baseImage == "" {
		baseImage = e.defaultImage
	}
	if baseImage == "" {
		baseImage = defaultImage
	}
	addl.image = baseImage

	for _, dstep := range dwf.Steps {
		swf.Steps = append(swf.Steps, Step{
			name:        dstep.Name,
			command:     dstep.Command,
			kind:        models.StepKindUser,
			environment: dstep.Environment,
		})
	}

	swf.Name = twf.Name
	swf.Environment = dwf.Environment

	var setup []models.Step
	setup = append(setup, models.BuildCloneStep(twf, *tpl.TriggerMetadata, e.dev))
	swf.Steps = append(setup, swf.Steps...)

	swf.Data = addl
	return swf, nil
}

func (e *Engine) WorkflowTimeout() time.Duration {
	workflowTimeout, err := time.ParseDuration(e.workflowTimeout)
	if err != nil {
		e.l.Error("failed to parse workflow timeout", "error", err, "timeout", e.workflowTimeout)
		workflowTimeout = 5 * time.Minute
	}
	return workflowTimeout
}

func (e *Engine) SetupWorkflow(ctx context.Context, wid models.WorkflowId, wf *models.Workflow, wfLogger models.WorkflowLogger) error {
	l := e.l.With("workflow", wid)
	l.Info("setting up workflow")

	setupStep := Step{
		name: "docker image pull",
		kind: models.StepKindSystem,
	}
	setupStepIdx := -1

	wfLogger.ControlWriter(setupStepIdx, setupStep, models.StepStatusStart).Write([]byte{0})
	defer wfLogger.ControlWriter(setupStepIdx, setupStep, models.StepStatusEnd).Write([]byte{0})

	if _, err := e.docker.NetworkCreate(ctx, networkName(wid), network.CreateOptions{
		Driver: "bridge",
	}); err != nil {
		return err
	}
	e.registerCleanup(wid, func(ctx context.Context) error {
		if err := e.docker.NetworkRemove(ctx, networkName(wid)); err != nil {
			return fmt.Errorf("removing network: %w", err)
		}
		return nil
	})

	addl := wf.Data.(addlFields)
	l.Info("pulling image", "image", addl.image)
	fmt.Fprintf(wfLogger.DataWriter(setupStepIdx, "stdout"), "pulling image: %s", addl.image)

	reader, err := e.docker.ImagePull(ctx, addl.image, image.PullOptions{})
	if err != nil {
		l.Error("image pull failed", "error", err.Error())
		fmt.Fprintf(wfLogger.DataWriter(setupStepIdx, "stderr"), "image pull failed: %s", err)
		return fmt.Errorf("pulling image: %w", err)
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		wfLogger.DataWriter(setupStepIdx, "stdout").Write([]byte(line))
		l.Info("image pull progress", "stdout", line)
	}

	l.Info("creating container")
	wfLogger.DataWriter(setupStepIdx, "stdout").Write([]byte("creating container..."))

	resp, err := e.docker.ContainerCreate(ctx, &container.Config{
		Image:      addl.image,
		Cmd:        []string{"cat"},
		OpenStdin:  true,
		Tty:        false,
		Hostname:   "spindle",
		WorkingDir: workspaceDir,
		Labels: map[string]string{
			"sh.tangled.pipeline/workflow_id": wid.String(),
		},
	}, &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeTmpfs,
				Target: "/tmp",
				TmpfsOptions: &mount.TmpfsOptions{
					Mode: 0o1777,
					Options: [][]string{
						{"exec"},
					},
				},
			},
		},
		ReadonlyRootfs: false,
		CapDrop:        []string{"ALL"},
		CapAdd:         []string{"CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER", "CAP_SETUID", "CAP_SETGID"},
		SecurityOpt:    []string{"no-new-privileges"},
		ExtraHosts:     []string{"host.docker.internal:host-gateway"},
	}, nil, nil, "")
	if err != nil {
		fmt.Fprintf(wfLogger.DataWriter(setupStepIdx, "stderr"), "container creation failed: %s", err)
		return fmt.Errorf("creating container: %w", err)
	}

	e.registerCleanup(wid, func(ctx context.Context) error {
		if err := e.docker.ContainerStop(ctx, resp.ID, container.StopOptions{}); err != nil {
			return fmt.Errorf("stopping container: %w", err)
		}
		if err := e.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{
			RemoveVolumes: true,
			Force:         false,
		}); err != nil {
			return fmt.Errorf("removing container: %w", err)
		}
		return nil
	})

	wfLogger.DataWriter(setupStepIdx, "stdout").Write([]byte("starting container..."))
	if err := e.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	mkExecResp, err := e.docker.ContainerExecCreate(ctx, resp.ID, container.ExecOptions{
		Cmd:          []string{"mkdir", "-p", workspaceDir, homeDir},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return err
	}

	execResp, err := e.docker.ContainerExecAttach(ctx, mkExecResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer execResp.Close()

	if _, err := io.ReadAll(execResp.Reader); err != nil {
		return err
	}

	execInspectResp, err := e.docker.ContainerExecInspect(ctx, mkExecResp.ID)
	if err != nil {
		return err
	}

	if execInspectResp.ExitCode != 0 {
		return fmt.Errorf("mkdir exited with exit code %d", execInspectResp.ExitCode)
	}
	if execInspectResp.Running {
		return errors.New("mkdir is somehow still running")
	}

	addl.container = resp.ID
	wf.Data = addl

	return nil
}

func (e *Engine) RunStep(ctx context.Context, wid models.WorkflowId, w *models.Workflow, idx int, secrets []secrets.UnlockedSecret, wfLogger models.WorkflowLogger) error {
	addl := w.Data.(addlFields)

	workflowEnvs := ConstructEnvs(w.Environment)
	for _, s := range secrets {
		workflowEnvs.AddEnv(s.Key, s.Value)
	}

	step := w.Steps[idx]

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	envs := append(EnvVars(nil), workflowEnvs...)
	if dockerStep, ok := step.(Step); ok {
		for k, v := range dockerStep.environment {
			envs.AddEnv(k, v)
		}
	}

	envs.AddEnv("HOME", homeDir)

	mkExecResp, err := e.docker.ContainerExecCreate(ctx, addl.container, container.ExecOptions{
		Cmd:          []string{"sh", "-c", step.Command()},
		AttachStdout: true,
		AttachStderr: true,
		Env:          envs,
	})
	if err != nil {
		return fmt.Errorf("creating exec: %w", err)
	}

	tailDone := make(chan error, 1)
	go func() {
		tailDone <- e.tailStep(ctx, wfLogger, mkExecResp.ID, idx)
	}()

	select {
	case <-tailDone:
	case <-ctx.Done():
		e.l.Warn("step timed out", "step", step.Name())
		<-tailDone
		return engine.ErrTimedOut
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	execInspectResp, err := e.docker.ContainerExecInspect(ctx, mkExecResp.ID)
	if err != nil {
		return err
	}

	if execInspectResp.ExitCode != 0 {
		inspectResp, err := e.docker.ContainerInspect(ctx, addl.container)
		if err != nil {
			return err
		}

		e.l.Error("workflow failed", "workflow_id", wid.String(), "exit_code", execInspectResp.ExitCode, "oom_killed", inspectResp.State.OOMKilled)

		if inspectResp.State.OOMKilled {
			return ErrOOMKilled
		}
		return engine.ErrWorkflowFailed
	}

	return nil
}

func (e *Engine) tailStep(ctx context.Context, wfLogger models.WorkflowLogger, execID string, stepIdx int) error {
	if wfLogger == nil {
		return nil
	}

	logs, err := e.docker.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer logs.Close()

	_, err = stdcopy.StdCopy(
		wfLogger.DataWriter(stepIdx, "stdout"),
		wfLogger.DataWriter(stepIdx, "stderr"),
		logs.Reader,
	)
	if err != nil && err != io.EOF && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("failed to copy logs: %w", err)
	}

	return nil
}

func (e *Engine) DestroyWorkflow(ctx context.Context, wid models.WorkflowId) error {
	fns := e.drainCleanups(wid)
	for _, fn := range fns {
		if err := fn(ctx); err != nil {
			e.l.Error("failed to cleanup workflow resource", "workflowId", wid, "error", err)
		}
	}
	return nil
}

func (e *Engine) registerCleanup(wid models.WorkflowId, fn cleanupFunc) {
	e.cleanupMu.Lock()
	defer e.cleanupMu.Unlock()
	key := wid.String()
	e.cleanup[key] = append(e.cleanup[key], fn)
}

func (e *Engine) drainCleanups(wid models.WorkflowId) []cleanupFunc {
	e.cleanupMu.Lock()
	key := wid.String()
	fns := e.cleanup[key]
	delete(e.cleanup, key)
	e.cleanupMu.Unlock()
	return fns
}

func networkName(wid models.WorkflowId) string {
	return fmt.Sprintf("workflow-network-%s", wid)
}
