// Copyright 2024 Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/daytonaio/daytona/pkg/logger"
	"github.com/daytonaio/daytona/pkg/workspace"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

type BuildOutcome struct {
	Outcome   string   `json:"outcome"`
	ImageName []string `json:"imageName"`
}

type DevcontainerBuilderConfig struct {
	buildId                      string
	project                      workspace.Project
	loggerFactory                logger.LoggerFactory
	localContainerRegistryServer string
	projectVolumePath            string
}

type DevcontainerBuilder struct {
	BuilderPlugin
	DevcontainerBuilderConfig
	buildImageName string
}

func (b *DevcontainerBuilder) Build() (*BuildResult, error) {
	err := b.startContainer()
	if err != nil {
		return nil, err
	}

	err = b.startDocker()
	if err != nil {
		return nil, err
	}

	err = b.buildDevcontainer()
	if err != nil {
		return nil, err
	}

	remoteUser, err := b.getRemoteUser()
	if err != nil {
		return nil, err
	}

	return &BuildResult{
		User:              remoteUser,
		ImageName:         b.buildImageName,
		ProjectVolumePath: b.projectVolumePath,
	}, nil
}

func (b *DevcontainerBuilder) CleanUp() error {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	err = cli.ContainerRemove(ctx, b.buildId, container.RemoveOptions{
		Force: true,
	})
	if err != nil {
		return err
	}

	err = os.RemoveAll(b.projectVolumePath)
	if err != nil {
		return err
	}

	return nil
}

func (b *DevcontainerBuilder) Publish() error {
	projectLogger := b.loggerFactory.CreateProjectLogger(b.project.WorkspaceId, b.project.Name)
	defer projectLogger.Close()

	ctx := context.Background()
	cliBuilder, err := b.getBuilderDockerClient()
	if err != nil {
		return err
	}

	reader, err := cliBuilder.ImagePush(ctx, b.buildImageName, image.PushOptions{
		//	todo: registry auth (from container registry store)
		RegistryAuth: "empty", //	make sure that something is passed, as with "" will throw an X-Auth error
	})
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		projectLogger.Write([]byte(scanner.Text() + "\n"))
	}

	return nil
}

func (b *DevcontainerBuilder) buildDevcontainer() error {
	projectLogger := b.loggerFactory.CreateProjectLogger(b.project.WorkspaceId, b.project.Name)
	defer projectLogger.Close()

	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	cmd := []string{"devcontainer", "build", "--workspace-folder", "/project"}
	if b.project.Build.Devcontainer.DevContainerFilePath != "" {
		cmd = append(cmd, "--config", filepath.Join("/project", b.project.Build.Devcontainer.DevContainerFilePath))
	}

	execConfig := types.ExecConfig{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	}

	execIDResp, err := cli.ContainerExecCreate(ctx, b.buildId, execConfig)
	if err != nil {
		return err
	}

	attachResp, err := cli.ContainerExecAttach(ctx, execIDResp.ID, types.ExecStartCheck{})
	if err != nil {
		return err
	}
	defer attachResp.Close()

	var buildOutcome BuildOutcome
	var lastLine string
	scanner := bufio.NewScanner(attachResp.Reader)
	for scanner.Scan() {
		lastLine = scanner.Text()
		projectLogger.Write([]byte(lastLine + "\n"))

		if strings.Contains(lastLine, `{"outcome"`) {
			start := strings.Index(lastLine, "{")
			end := strings.LastIndex(lastLine, "}")
			if start != -1 && end != -1 && start < end {
				lastLine = lastLine[start : end+1]
			}

			err = json.Unmarshal([]byte(lastLine), &buildOutcome)
			if err != nil {
				// lastLine could not be unmarshalled into the expected JSON structure
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if buildOutcome.Outcome != "success" {
		//	todo: parse reason
		return errors.New("devcontainer build failed")
	}

	execInspect, err := cli.ContainerExecInspect(ctx, execIDResp.ID)
	if err != nil {
		return err
	}
	if execInspect.ExitCode != 0 {
		return errors.New("devcontainer build failed")
	}

	devcontainerImage := buildOutcome.ImageName[0]

	//	todo: commit sha from project Repository
	tag := "latest"
	imageName := fmt.Sprintf("%s:%s", b.localContainerRegistryServer+"/p-"+b.buildId, tag)

	cliBuilder, err := b.getBuilderDockerClient()
	if err != nil {
		return err
	}

	err = cliBuilder.ImageTag(ctx, devcontainerImage, imageName)
	if err != nil {
		return err
	}

	b.buildImageName = imageName

	return nil
}

func (b *DevcontainerBuilder) getRemoteUser() (string, error) {
	ctx := context.Background()
	cliBuilder, err := b.getBuilderDockerClient()
	if err != nil {
		return "", err
	}

	imageInspect, _, err := cliBuilder.ImageInspectWithRaw(ctx, b.buildImageName)
	if err != nil {
		return "", err
	}
	meta, ok := imageInspect.Config.Labels["devcontainer.metadata"]
	if !ok {
		return "", errors.New("devcontainer.metadata label not found")
	}
	var metadata []interface{}
	err = json.Unmarshal([]byte(meta), &metadata)
	if err != nil {
		return "", err
	}
	for _, m := range metadata {
		metadataMap := m.(map[string]interface{})
		if metadataMap["remoteUser"] != nil {
			return metadataMap["remoteUser"].(string), nil
		}
	}
	return "", errors.New("remoteUser not found in metadata")
}

func (b *DevcontainerBuilder) startContainer() error {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	//	todo builder image from config
	reader, err := cli.ImagePull(ctx, "daytonaio/workspace-project", image.PullOptions{
		//	auth for builder image
		//	RegistryAuth: ,
	})
	if err != nil {
		return err
	}
	//	wait for pull to complete
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return err
	}

	dir := "/tmp/" + b.buildId + "/etc/docker"
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}

	//	todo: mount folders
	_, err = cli.ContainerCreate(ctx, &container.Config{
		Image:      "daytonaio/workspace-project",
		Entrypoint: []string{"sleep", "infinity"},
	}, &container.HostConfig{
		NetworkMode: "host",
		Privileged:  true,
		Binds: []string{
			b.projectVolumePath + ":/project",
			filepath.Dir(b.getLocalDockerSocket()) + ":/tmp/docker",
		},
	}, nil, nil, b.buildId)
	if err != nil {
		return err
	}

	if err := cli.ContainerStart(ctx, b.buildId, container.StartOptions{}); err != nil {
		return err
	}

	return nil
}

func (b *DevcontainerBuilder) startDocker() error {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	execConfig := types.ExecConfig{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"dockerd", "-H", "unix:///tmp/docker/docker.sock", "-H", "unix:///var/run/docker.sock", "--insecure-registry", "localhost:5000"},
	}

	execResp, err := cli.ContainerExecCreate(ctx, b.buildId, execConfig)
	if err != nil {
		return err
	}

	execStartCheck := types.ExecStartCheck{
		Detach: false,
		Tty:    false,
	}

	attachResp, err := cli.ContainerExecAttach(ctx, execResp.ID, execStartCheck)
	if err != nil {
		return err
	}
	defer attachResp.Close()

	go func() {
		_, err = io.Copy(os.Stdout, attachResp.Reader)
		if err != nil {
			log.Errorf("error copying output: %v", err)
		}
	}()

	return nil
}

func (b *DevcontainerBuilder) getBuilderDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.WithHost(fmt.Sprintf("unix://%s", b.getLocalDockerSocket())), client.WithAPIVersionNegotiation())
}

func (b *DevcontainerBuilder) getLocalDockerSocket() string {
	return filepath.Join("/tmp", b.buildId, "docker", "docker.sock")
}
